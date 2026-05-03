package status

import (
	"maps"
	"slices"
	"sync"
	"time"
)

const defaultEventLimit = 50

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
	PiType           string
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

// ReconcileEvent is a compact in-memory event rendered by the dashboard.
type ReconcileEvent struct {
	At      time.Time
	Result  string
	SHA     string
	Message string
}

// Snapshot is a point-in-time copy of agent runtime status.
type Snapshot struct {
	Units        map[string]UnitRuntimeStatus
	Dependencies map[string]UnitDependencies
	Host         HostMetadata
	OrphanScan   OrphanScan
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
		Dependencies: cloneDependenciesMap(in.Dependencies),
		Host:         cloneHost(in.Host),
		OrphanScan:   in.OrphanScan,
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
		PiType:           in.PiType,
		Features:         slices.Clone(in.Features),
		ExternalHostname: in.ExternalHostname,
	}
}
