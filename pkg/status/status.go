package status

import (
	"maps"
	"slices"
	"sync"
	"time"
)

const (
	defaultEventLimit = 50
	// resultSuccess is systemd's Result= value for a run that completed cleanly.
	resultSuccess = "success"
)

// UnitRuntimeStatus is the current systemd state for a managed unit.
type UnitRuntimeStatus struct {
	ActiveState string
	SubState    string
}

// UnitDependencies contains generated and declared systemd dependencies for a unit.
type UnitDependencies struct {
	Requires []string
	Wants    []string
	After    []string
	Before   []string
	BindsTo  []string
	PartOf   []string
}

// IsEmpty reports whether the dependencies struct contains no targets.
func (d UnitDependencies) IsEmpty() bool {
	return len(d.Requires)+len(d.Wants)+len(d.After)+len(d.Before)+len(d.BindsTo)+len(d.PartOf) == 0
}

// HostMetadata describes the resolved host config for the current agent host.
type HostMetadata struct {
	Role             string
	Features         []string
	ExternalHostname string
}

// OrphanScan captures the most recent startup orphan cleanup outcome.
type OrphanScan struct {
	Ran            bool
	FilesRemoved   int
	SecretsRemoved int
	Error          string
}

// PruneStatus captures the most recent image-prune outcome.
//   - LastRunAt is the last *successful* (fully clean) prune; a zero value means
//     none is known in this process's view and no last-prune-timestamp series is
//     emitted.
//   - ImagesRemoved/ReclaimedBytes report what the most recent attempt reclaimed.
//     They are also populated on a partial failure, so they are not necessarily
//     tied to LastRunAt.
//   - LastErrorAt/Error record the most recent failed attempt.
type PruneStatus struct {
	LastRunAt      time.Time
	ImagesRemoved  int
	ReclaimedBytes uint64
	LastErrorAt    time.Time
	Error          string
}

// RunObservation is one health pass's raw view of a unit's last run, straight
// from systemd. A zero time means systemd reports no such event yet.
type RunObservation struct {
	// StartedAt is when the last run started (Unit InactiveExitTimestamp).
	StartedAt time.Time
	// FinishedAt is when the last run finished, whatever the outcome
	// (Unit InactiveEnterTimestamp). It doubles as the change detector for
	// "a run completed since the previous pass".
	FinishedAt time.Time
	// Result is the service's current Result= ("success", "exit-code",
	// "timeout", ...). systemd resets it to "success" when a run starts, so while
	// a run is in flight it describes that run's provisional state rather than the
	// outcome of the previous one; ObserveRun therefore never derives last-success
	// from it in that window.
	Result string
	// TriggeredAt is when a .timer last fired (Timer LastTriggerUSec); zero for
	// service units.
	TriggeredAt time.Time
}

// UnitRun is the retained run bookkeeping for one timer-triggered one-shot, or
// the last-trigger time for one managed .timer. It is what the metrics collector
// exports and it outlives a failed observation: only PruneRuns removes a record.
type UnitRun struct {
	RunObservation

	// SucceededAt is the FinishedAt of the last run that ended in "success",
	// derived across observations by ObserveRun. Zero means "never seen to
	// succeed" and the corresponding series is omitted rather than zeroed.
	SucceededAt time.Time
}

// ReconcileEvent is a compact in-memory event rendered by the dashboard.
type ReconcileEvent struct {
	At      time.Time
	Result  string
	SHA     string
	Message string
}

// Snapshot is a point-in-time copy of agent runtime status.
type Snapshot struct {
	Units map[string]UnitRuntimeStatus
	// Runs holds run bookkeeping for timer-triggered one-shots and managed
	// .timers. Unlike Units it is merged, never replaced: see ObserveRun.
	Runs         map[string]UnitRun
	Dependencies map[string]UnitDependencies
	Host         HostMetadata
	OrphanScan   OrphanScan
	Prune        PruneStatus
	Events       []ReconcileEvent
	VerifiedAt   time.Time
	// Bootstrapped is true once the agent has completed at least one
	// resolve+analyze cycle. The first SetDependencies call flips it.
	Bootstrapped bool
}

// Store keeps the latest runtime status in memory.
type Store struct {
	mu         sync.RWMutex
	snapshot   Snapshot
	eventLimit int
}

// NewStore creates an empty status store.
func NewStore() *Store {
	return &Store{eventLimit: defaultEventLimit}
}

// Snapshot returns a deep copy of the current runtime status.
func (s *Store) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSnapshot(s.snapshot)
}

// SetUnits replaces the unit runtime status snapshot.
func (s *Store) SetUnits(units map[string]UnitRuntimeStatus) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Units = cloneUnits(units)
}

// SetUnit sets the runtime status for a single unit.
func (s *Store) SetUnit(unit string, st UnitRuntimeStatus) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.Units == nil {
		s.snapshot.Units = make(map[string]UnitRuntimeStatus)
	}
	s.snapshot.Units[unit] = st
}

// DeleteUnit removes one unit from the runtime status snapshot.
func (s *Store) DeleteUnit(unit string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snapshot.Units, unit)
}

// ClearUnits clears all unit runtime statuses.
func (s *Store) ClearUnits() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.snapshot.Units)
}

// ObserveRun merges one health pass's observation of a timer-triggered one-shot
// (or a managed .timer) into the retained record and derives last-success.
//
// Run records deliberately do not share the Units lifecycle: SetUnits replaces
// and ClearUnits empties that map, whereas a run record survives a failed
// per-unit query and an all-failed pass — the series they feed are "how long ago
// did this job last succeed", which must not flap on a D-Bus hiccup. Only
// PruneRuns removes a record.
//
// Result follows the observation: it is the unit's current systemd Result=, which
// systemd resets to "success" the moment a run starts. Only a unit that has never
// run reports no result at all, because systemd initialises Result= to "success"
// before there is any outcome to report.
//
// Last success, by contrast, is derived across observations precisely because of
// that reset: it advances only when the finish timestamp moved since the previous
// observation, no run started after that finish (StartedAt after FinishedAt means
// one is in flight, so Result belongs to it and not to the finished run), and the
// result was "success". Anything else keeps the previous last-success — on a first
// observation that means publishing none at all, which is honest: systemd has
// already overwritten whatever the last completed run reported.
func (s *Store) ObserveRun(unit string, obs RunObservation) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.Runs == nil {
		s.snapshot.Runs = make(map[string]UnitRun)
	}
	prev := s.snapshot.Runs[unit]
	run := UnitRun{RunObservation: obs, SucceededAt: prev.SucceededAt}
	if obs.StartedAt.IsZero() {
		// Never ran: systemd's initial Result="success" is not an outcome.
		run.Result = ""
	}
	if !obs.FinishedAt.Equal(prev.FinishedAt) &&
		!obs.StartedAt.After(obs.FinishedAt) &&
		obs.Result == resultSuccess {
		run.SucceededAt = obs.FinishedAt
	}
	s.snapshot.Runs[unit] = run
}

// PruneRuns drops run records for units outside keep, the Fleet's current unit
// set. Leaving the Fleet is the only reason a record disappears.
func (s *Store) PruneRuns(keep []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for unit := range s.snapshot.Runs {
		if !slices.Contains(keep, unit) {
			delete(s.snapshot.Runs, unit)
		}
	}
}

// SetDependencies replaces the generated dependency snapshot.
//
// On the first call this also flips Snapshot.Bootstrapped to true — the agent
// uses this as the signal that runtime status has been populated at least
// once (resolve + analyze completed without error).
func (s *Store) SetDependencies(deps map[string]UnitDependencies) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Dependencies = cloneDependenciesMap(deps)
	s.snapshot.Bootstrapped = true
}

// SetHost replaces resolved host metadata.
func (s *Store) SetHost(host HostMetadata) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Host = cloneHost(host)
}

// SetOrphanScan records the latest orphan scan result.
func (s *Store) SetOrphanScan(scan OrphanScan) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.OrphanScan = scan
}

// SetPrune replaces the recorded image-prune status. Any merge policy (e.g.
// preserving the last-success timestamp across a failed attempt) is owned by the
// caller, mirroring how SetOrphanScan is used.
func (s *Store) SetPrune(prune PruneStatus) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Prune = prune
}

// Prune returns the current image-prune status. It is a cheap value copy (no
// map/slice cloning), suitable for the metrics scrape path and for a caller that
// needs to merge a new result with the existing one.
func (s *Store) Prune() PruneStatus {
	if s == nil {
		return PruneStatus{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot.Prune
}

// SetVerifiedAt records the last successful verification time.
func (s *Store) SetVerifiedAt(t time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.VerifiedAt = t
}

// AddEvent appends an event and trims the ring to the configured limit.
func (s *Store) AddEvent(event ReconcileEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Events = append(s.snapshot.Events, event)
	limit := s.eventLimit
	if limit <= 0 {
		limit = defaultEventLimit
	}
	if extra := len(s.snapshot.Events) - limit; extra > 0 {
		s.snapshot.Events = slices.Delete(s.snapshot.Events, 0, extra)
	}
}

func cloneSnapshot(in Snapshot) Snapshot {
	return Snapshot{
		Units:        cloneUnits(in.Units),
		Runs:         maps.Clone(in.Runs),
		Dependencies: cloneDependenciesMap(in.Dependencies),
		Host:         cloneHost(in.Host),
		OrphanScan:   in.OrphanScan,
		Prune:        in.Prune,
		Events:       slices.Clone(in.Events),
		VerifiedAt:   in.VerifiedAt,
		Bootstrapped: in.Bootstrapped,
	}
}

func cloneUnits(in map[string]UnitRuntimeStatus) map[string]UnitRuntimeStatus {
	return maps.Clone(in)
}

func cloneDependenciesMap(in map[string]UnitDependencies) map[string]UnitDependencies {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]UnitDependencies, len(in))
	for k, v := range in {
		out[k] = cloneDependencies(v)
	}
	return out
}

func cloneDependencies(in UnitDependencies) UnitDependencies {
	return UnitDependencies{
		Requires: slices.Clone(in.Requires),
		Wants:    slices.Clone(in.Wants),
		After:    slices.Clone(in.After),
		Before:   slices.Clone(in.Before),
		BindsTo:  slices.Clone(in.BindsTo),
		PartOf:   slices.Clone(in.PartOf),
	}
}

func cloneHost(in HostMetadata) HostMetadata {
	return HostMetadata{
		Role:             in.Role,
		Features:         slices.Clone(in.Features),
		ExternalHostname: in.ExternalHostname,
	}
}
