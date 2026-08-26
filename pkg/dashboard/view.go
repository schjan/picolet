package dashboard

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
)

const (
	failureGateThreshold = 3
	failureGateExpiry    = time.Hour // mirrors pkg/agent failure-gate window
	refreshSeconds       = 30
)

// HeaderInput is the raw set of fields the handler hands to buildViewModel.
type HeaderInput struct {
	Hostname         string
	Version          string
	AppliedSHA       string
	AppliedAt        time.Time
	VerifiedAt       time.Time
	Role             string
	Features         []string
	ExternalHostname string
	FailedSHA        string
	FailedCount      int
	FailedAt         time.Time
}

// Header is the rendered header data.
type Header struct {
	Hostname         string
	Version          string
	AppliedSHAShort  string
	AppliedAgo       string
	VerifiedAgo      string
	Role             string
	Features         string
	ExternalHostname string
}

// Banner describes the failure-gate banner state.
type Banner struct {
	Active         bool
	FailedSHAShort string
	FailedCount    int
	FailedAgo      string
}

// ViewModel is the value passed to the index template.
type ViewModel struct {
	Header         Header
	Banner         *Banner
	Groups         []CategoryGroup
	OrphanScan     *OrphanScan
	RecentEvents   []RecentEvent
	RenderedAt     time.Time
	RefreshSec     int
	RefreshEnabled bool
}

// Status describes the visual treatment of a unit's runtime state.
type Status struct {
	Glyph string
	Token string
	Class string // CSS modifier — used as status--<Class>
}

// UnitRow is one row in a category table.
type UnitRow struct {
	Path         string
	Basename     string
	HashShort    string
	Service      string
	Status       Status
	SubState     string
	Dependencies []DependencyGroup
}

// CategoryGroup bundles rows under one apply-phase heading.
type CategoryGroup struct {
	Category string
	Rows     []UnitRow
}

// DependencyGroup is a rendered relation and its dependency targets.
type DependencyGroup struct {
	Relation string
	Targets  []string
}

// OrphanScan is the dashboard rendering shape for startup orphan cleanup.
type OrphanScan struct {
	FilesRemoved   int
	SecretsRemoved int
	Error          string
}

// RecentEvent is one process-local dashboard event.
type RecentEvent struct {
	Result  string
	When    string
	SHA     string
	Message string
}

// shortHash strips the "sha256:" prefix that pkg/reconciler emits, then
// truncates to 8 chars.
func shortHash(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func relativeTime(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < 30*time.Second:
		return "just now"
	case d <= 90*time.Second:
		return "1 minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d <= 90*time.Minute:
		return "1 hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "1 day ago"
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

func statusFromActiveState(s string) Status {
	switch s {
	case "active":
		return Status{Glyph: "█", Token: "active", Class: "ok"}
	case "activating":
		return Status{Glyph: "▚", Token: "activating", Class: "pending"}
	case "reloading":
		return Status{Glyph: "▚", Token: "reloading", Class: "pending"}
	case "deactivating":
		return Status{Glyph: "▚", Token: "deactivating", Class: "pending"}
	case "inactive":
		return Status{Glyph: "░", Token: "inactive", Class: "cold"}
	case "failed":
		return Status{Glyph: "▌", Token: "failed", Class: "bad"}
	default:
		return Status{Glyph: "?", Token: "unknown", Class: "unknown"}
	}
}

func groupByCategory(files map[string]state.ManagedFile, services map[string]string) []CategoryGroup {
	buckets := map[string][]UnitRow{}
	for path, mf := range files {
		category := mf.Category.String()
		buckets[category] = append(buckets[category], UnitRow{
			Path:      path,
			Basename:  filepath.Base(path),
			HashShort: shortHash(mf.Hash),
			Service:   services[path],
		})
	}
	var out []CategoryGroup
	for _, cat := range applier.CategoryOrder() {
		category := cat.String()
		if rows, ok := buckets[category]; ok {
			slices.SortFunc(rows, func(a, b UnitRow) int { return cmp.Compare(a.Basename, b.Basename) })
			out = append(out, CategoryGroup{Category: category, Rows: rows})
			delete(buckets, category)
		}
	}
	if len(buckets) > 0 {
		var leftovers []UnitRow
		for _, rows := range buckets {
			leftovers = append(leftovers, rows...)
		}
		slices.SortFunc(leftovers, func(a, b UnitRow) int { return cmp.Compare(a.Basename, b.Basename) })
		out = append(out, CategoryGroup{Category: "other", Rows: leftovers})
	}
	return out
}

func buildViewModel(
	in HeaderInput,
	files map[string]state.ManagedFile,
	services map[string]string,
	statuses map[string]status.UnitRuntimeStatus,
	deps map[string]status.UnitDependencies,
	orphan status.OrphanScan,
	events []status.ReconcileEvent,
	now time.Time,
	refreshEnabled bool,
) ViewModel {
	groups := groupByCategory(files, services)
	for gi := range groups {
		cat := groups[gi].Category
		for ri := range groups[gi].Rows {
			resolveRowStatus(&groups[gi].Rows[ri], cat, statuses)
			groups[gi].Rows[ri].Dependencies = dependencyGroups(deps[groups[gi].Rows[ri].Service])
		}
	}

	return ViewModel{
		Header: Header{
			Hostname:         in.Hostname,
			Version:          in.Version,
			AppliedSHAShort:  shortSHA(in.AppliedSHA),
			AppliedAgo:       relativeTime(in.AppliedAt, now),
			VerifiedAgo:      relativeTime(in.VerifiedAt, now),
			Role:             in.Role,
			Features:         strings.Join(in.Features, ", "),
			ExternalHostname: in.ExternalHostname,
		},
		Banner:         buildBanner(in, now),
		Groups:         groups,
		OrphanScan:     buildOrphanScan(orphan),
		RecentEvents:   buildRecentEvents(events, now),
		RenderedAt:     now,
		RefreshSec:     refreshSeconds,
		RefreshEnabled: refreshEnabled,
	}
}

var mutedStatus = Status{Glyph: "·", Token: "—", Class: "muted"}

// unitNameFor returns the systemd unit name for a managed file, or "" for
// categories that legitimately have no associated unit (manifest, secret).
// Mirrors applier.unitNameForDelete; kept private here to avoid exporting
// applier internals just for the dashboard.
func unitNameFor(category, path, mappedService string) string {
	switch category {
	case "container", "network", "volume", "kube":
		return mappedService
	case "systemd":
		if mappedService != "" {
			return mappedService
		}
		return filepath.Base(path)
	default:
		return ""
	}
}

func resolveRowStatus(row *UnitRow, category string, statuses map[string]status.UnitRuntimeStatus) {
	unit := unitNameFor(category, row.Path, row.Service)
	if unit == "" {
		row.Status = mutedStatus
		return
	}
	// Write the derived name back so the template's "unit" column always shows a
	// unit name (defensive: covers any managed file whose ServiceName is unset).
	row.Service = unit
	st, ok := statuses[unit]
	if !ok {
		row.Status = statusFromActiveState("")
		return
	}
	row.Status = statusFromActiveState(st.ActiveState)
	row.SubState = st.SubState
}

func buildBanner(in HeaderInput, now time.Time) *Banner {
	if in.FailedCount < failureGateThreshold ||
		in.FailedAt.IsZero() ||
		now.Sub(in.FailedAt) >= failureGateExpiry {
		return nil
	}
	return &Banner{
		Active:         true,
		FailedSHAShort: shortSHA(in.FailedSHA),
		FailedCount:    in.FailedCount,
		FailedAgo:      relativeTime(in.FailedAt, now),
	}
}

func dependencyGroups(deps status.UnitDependencies) []DependencyGroup {
	groups := []DependencyGroup{
		{Relation: "requires", Targets: deps.Requires},
		{Relation: "wants", Targets: deps.Wants},
		{Relation: "after", Targets: deps.After},
		{Relation: "before", Targets: deps.Before},
		{Relation: "binds to", Targets: deps.BindsTo},
		{Relation: "part of", Targets: deps.PartOf},
	}
	out := groups[:0]
	for _, group := range groups {
		if len(group.Targets) > 0 {
			out = append(out, group)
		}
	}
	return out
}

// buildOrphanScan returns nil for both "not run" and "ran but cleaned nothing"
// — a panel that only ever says "0 files · 0 secrets" is noise. The panel
// only appears when there is a non-zero result or an error worth surfacing.
func buildOrphanScan(orphan status.OrphanScan) *OrphanScan {
	if !orphan.Ran {
		return nil
	}
	if orphan.FilesRemoved == 0 && orphan.SecretsRemoved == 0 && orphan.Error == "" {
		return nil
	}
	return &OrphanScan{
		FilesRemoved:   orphan.FilesRemoved,
		SecretsRemoved: orphan.SecretsRemoved,
		Error:          orphan.Error,
	}
}

func buildRecentEvents(events []status.ReconcileEvent, now time.Time) []RecentEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]RecentEvent, 0, len(events))
	for _, event := range slices.Backward(events) {
		out = append(out, RecentEvent{
			Result:  event.Result,
			When:    relativeTime(event.At, now),
			SHA:     shortSHA(event.SHA),
			Message: event.Message,
		})
	}
	return out
}
