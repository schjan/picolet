package dashboard

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/schjan/picolet/pkg/state"
)

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
