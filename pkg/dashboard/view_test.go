package dashboard

import (
	"reflect"
	"testing"
	"time"

	"github.com/schjan/picolet/pkg/state"
)

func TestShortHash(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                                                                      "",
		"abc":                                                                   "abc",
		"0123456789abcdef":                                                      "01234567",
		"fffffffffffffffffffffffffffffffff":                                     "ffffffff",
		"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd": "01234567",
		"sha256:abcd": "abcd",
	}
	for in, want := range cases {
		if got := shortHash(in); got != want {
			t.Errorf("shortHash(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortSHA(t *testing.T) {
	t.Parallel()
	if got := shortSHA("a1b2c3d4e5f6"); got != "a1b2c3d" {
		t.Errorf("shortSHA = %q", got)
	}
	if got := shortSHA(""); got != "" {
		t.Errorf("shortSHA(empty) = %q", got)
	}
}

func TestRelativeTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Time{}, ""},
		{now.Add(-5 * time.Second), "just now"},
		{now.Add(-90 * time.Second), "1 minute ago"},
		{now.Add(-3 * time.Minute), "3 minutes ago"},
		{now.Add(-90 * time.Minute), "1 hour ago"},
		{now.Add(-3 * time.Hour), "3 hours ago"},
		{now.Add(-26 * time.Hour), "1 day ago"},
		{now.Add(-72 * time.Hour), "3 days ago"},
	}
	for _, tc := range cases {
		if got := relativeTime(tc.in, now); got != tc.want {
			t.Errorf("relativeTime(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStatusFromActiveState(t *testing.T) {
	t.Parallel()
	cases := map[string]Status{
		"active":       {Glyph: "█", Token: "active", Class: "ok"},
		"activating":   {Glyph: "▚", Token: "activating", Class: "pending"},
		"reloading":    {Glyph: "▚", Token: "activating", Class: "pending"},
		"inactive":     {Glyph: "░", Token: "inactive", Class: "cold"},
		"failed":       {Glyph: "▌", Token: "failed", Class: "bad"},
		"":             {Glyph: "?", Token: "unknown", Class: "unknown"},
		"deactivating": {Glyph: "?", Token: "unknown", Class: "unknown"},
	}
	for in, want := range cases {
		if got := statusFromActiveState(in); got != want {
			t.Errorf("statusFromActiveState(%q) = %+v", in, got)
		}
	}
}

func TestGroupByCategory(t *testing.T) {
	t.Parallel()
	files := map[string]state.ManagedFile{
		"/p/web.container": {Hash: "h-web-aaaa", Category: "container"},
		"/p/db.container":  {Hash: "h-db-aaaa", Category: "container"},
		"/p/lan.network":   {Hash: "h-lan-aaaa", Category: "network"},
		"/p/data.volume":   {Hash: "h-data-aaaa", Category: "volume"},
	}
	services := map[string]string{
		"/p/web.container": "web.service",
		"/p/db.container":  "db.service",
		"/p/lan.network":   "lan-network.service",
	}

	got := groupByCategory(files, services)

	var order []string
	for _, g := range got {
		order = append(order, g.Category)
	}
	if want := []string{"network", "volume", "container"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("category order = %v, want %v", order, want)
	}
	container := got[2]
	if container.Rows[0].Basename != "db.container" || container.Rows[1].Basename != "web.container" {
		t.Errorf("container rows not sorted: %+v", container.Rows)
	}
	if container.Rows[0].Service != "db.service" {
		t.Errorf("db service mapping missing")
	}
}
