package dashboard

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/state"
)

const (
	failureGateThreshold = 3
	failureGateExpiry    = time.Hour // mirrors pkg/agent failure-gate window
	refreshSeconds       = 30
)

// HeaderInput is the raw set of fields the handler hands to buildViewModel.
type HeaderInput struct {
	Hostname    string
	Version     string
	AppliedSHA  string
	AppliedAt   time.Time
	FailedSHA   string
	FailedCount int
	FailedAt    time.Time
}

// Header is the rendered header data.
type Header struct {
	Hostname        string
	Version         string
	AppliedSHAShort string
	AppliedAt       time.Time
	AppliedAgo      string
}

// Banner describes the failure-gate banner state.
type Banner struct {
	Active         bool
	FailedSHAShort string
	FailedCount    int
	FailedAt       time.Time
	FailedAgo      string
}

// ViewModel is the value passed to the index template.
type ViewModel struct {
	Header     Header
	Banner     *Banner
	Groups     []CategoryGroup
	RenderedAt time.Time
	RefreshSec int
}

// muteCategories are managed-file categories that legitimately have no systemd unit.
var muteCategories = map[string]bool{"manifest": true, "secret": true}

// Status describes the visual treatment of a unit's runtime state.
type Status struct {
	Glyph string
	Token string
	Class string // CSS modifier — used as status--<Class>
}

// UnitRow is one row in a category table.
type UnitRow struct {
	Path      string
	Basename  string
	HashShort string
	Service   string
	Status    Status
	SubState  string
}

// CategoryGroup bundles rows under one apply-phase heading.
type CategoryGroup struct {
	Category string
	Rows     []UnitRow
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
	case "activating", "reloading":
		return Status{Glyph: "▚", Token: "activating", Class: "pending"}
	case "inactive":
		return Status{Glyph: "░", Token: "inactive", Class: "cold"}
	case "failed":
		return Status{Glyph: "▌", Token: "failed", Class: "bad"}
	default:
		return Status{Glyph: "?", Token: "unknown", Class: "unknown"}
	}
}

// categoryOrder mirrors pkg/applier so the dashboard groups follow the apply order.
var categoryOrder = []string{"network", "volume", "secret", "systemd", "manifest", "container", "kube"}

func groupByCategory(files map[string]state.ManagedFile, services map[string]string) []CategoryGroup {
	buckets := map[string][]UnitRow{}
	for path, mf := range files {
		buckets[mf.Category] = append(buckets[mf.Category], UnitRow{
			Path:      path,
			Basename:  filepath.Base(path),
			HashShort: shortHash(mf.Hash),
			Service:   services[path],
		})
	}
	var out []CategoryGroup
	for _, cat := range categoryOrder {
		if rows, ok := buckets[cat]; ok {
			sort.Slice(rows, func(i, j int) bool { return rows[i].Basename < rows[j].Basename })
			out = append(out, CategoryGroup{Category: cat, Rows: rows})
			delete(buckets, cat)
		}
	}
	if len(buckets) > 0 {
		var leftovers []UnitRow
		for _, rows := range buckets {
			leftovers = append(leftovers, rows...)
		}
		sort.Slice(leftovers, func(i, j int) bool { return leftovers[i].Basename < leftovers[j].Basename })
		out = append(out, CategoryGroup{Category: "other", Rows: leftovers})
	}
	return out
}

func buildViewModel(
	in HeaderInput,
	files map[string]state.ManagedFile,
	services map[string]string,
	statuses map[string]applier.UnitStatus,
	now time.Time,
) ViewModel {
	groups := groupByCategory(files, services)
	for gi := range groups {
		cat := groups[gi].Category
		for ri := range groups[gi].Rows {
			row := &groups[gi].Rows[ri]
			if muteCategories[cat] {
				row.Status = Status{Glyph: "·", Token: "—", Class: "muted"}
				continue
			}
			// Derive unit name: prefer state's ServiceName mapping; for raw systemd
			// files (which resolver does not annotate), fall back to the file basename.
			// Write the derived name back onto row.Service so the template's "unit"
			// column shows the derived name, not "—".
			if row.Service == "" && cat == "systemd" {
				row.Service = row.Basename
			}
			if row.Service == "" {
				row.Status = Status{Glyph: "·", Token: "—", Class: "muted"}
				continue
			}
			st, ok := statuses[row.Service]
			if !ok {
				row.Status = statusFromActiveState("")
				continue
			}
			row.Status = statusFromActiveState(st.ActiveState)
			row.SubState = st.SubState
		}
	}

	var banner *Banner
	if in.FailedCount >= failureGateThreshold &&
		!in.FailedAt.IsZero() &&
		now.Sub(in.FailedAt) < failureGateExpiry {
		banner = &Banner{
			Active:         true,
			FailedSHAShort: shortSHA(in.FailedSHA),
			FailedCount:    in.FailedCount,
			FailedAt:       in.FailedAt,
			FailedAgo:      relativeTime(in.FailedAt, now),
		}
	}

	return ViewModel{
		Header: Header{
			Hostname:        in.Hostname,
			Version:         in.Version,
			AppliedSHAShort: shortSHA(in.AppliedSHA),
			AppliedAt:       in.AppliedAt,
			AppliedAgo:      relativeTime(in.AppliedAt, now),
		},
		Banner:     banner,
		Groups:     groups,
		RenderedAt: now,
		RefreshSec: refreshSeconds,
	}
}
