package dashboard

import (
	"reflect"
	"testing"
	"time"

	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
)

func TestShortHash(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                                  "",
		"abc":                               "abc",
		"0123456789abcdef":                  "01234567",
		"fffffffffffffffffffffffffffffffff": "ffffffff",
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
		"reloading":    {Glyph: "▚", Token: "reloading", Class: "pending"},
		"deactivating": {Glyph: "▚", Token: "deactivating", Class: "pending"},
		"inactive":     {Glyph: "░", Token: "inactive", Class: "cold"},
		"failed":       {Glyph: "▌", Token: "failed", Class: "bad"},
		"":             {Glyph: "?", Token: "unknown", Class: "unknown"},
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

// fixtureNow is the reference clock used by the buildViewModel tests below so
// each focused test stays small.
var fixtureNow = time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

// fixtureBuildViewModel returns the canonical "happy-path" inputs the
// buildViewModel tests share. Tests mutate fields on the returned struct as
// needed. Time anchor is the package-level fixtureNow.
func fixtureBuildViewModel() (HeaderInput, map[string]state.ManagedFile, map[string]string, map[string]status.UnitRuntimeStatus) {
	// Git commit SHAs are NOT sha256-prefixed — that prefix is only on
	// managed-file content hashes. shortSHA does not strip; shortHash does.
	in := HeaderInput{
		Hostname:    "pi-edge-01",
		Version:     "0.7.2",
		AppliedSHA:  "1a2b3c4d5e6f7g8",
		AppliedAt:   fixtureNow.Add(-3 * time.Minute),
		FailedSHA:   "deadbeefcafe",
		FailedCount: 4,
		FailedAt:    fixtureNow.Add(-30 * time.Second),
	}
	files := map[string]state.ManagedFile{
		"/p/web.container": {Hash: "sha256:abc12345aaaa", Category: "container"},
		"/p/data.volume":   {Hash: "sha256:vvvv1111aaaa", Category: "volume"},
		"/p/manifest.yml":  {Hash: "sha256:k8sk8s888888", Category: "manifest"},
	}
	services := map[string]string{
		"/p/web.container": "web.service",
		"/p/data.volume":   "data-volume.service",
	}
	statuses := map[string]status.UnitRuntimeStatus{
		"web.service":         {ActiveState: "active", SubState: "running"},
		"data-volume.service": {ActiveState: "failed", SubState: "dead"},
	}
	return in, files, services, statuses
}

func TestBuildViewModel_Header(t *testing.T) {
	t.Parallel()
	in, files, services, statuses := fixtureBuildViewModel()
	vm := buildViewModel(in, files, services, statuses, nil, status.OrphanScan{}, nil, fixtureNow, true)

	if vm.Header.Hostname != "pi-edge-01" {
		t.Errorf("hostname = %q", vm.Header.Hostname)
	}
	if vm.Header.AppliedSHAShort != "1a2b3c4" {
		t.Errorf("sha short = %q", vm.Header.AppliedSHAShort)
	}
	if vm.Header.AppliedAgo != "3 minutes ago" {
		t.Errorf("applied ago = %q", vm.Header.AppliedAgo)
	}
	if vm.RefreshSec != 30 {
		t.Errorf("refresh = %d", vm.RefreshSec)
	}
}

func TestBuildViewModel_BannerActive(t *testing.T) {
	t.Parallel()
	in, files, services, statuses := fixtureBuildViewModel()
	vm := buildViewModel(in, files, services, statuses, nil, status.OrphanScan{}, nil, fixtureNow, true)

	if vm.Banner == nil || !vm.Banner.Active {
		t.Fatal("banner should be active when FailedCount >= 3 within failure window")
	}
	if vm.Banner.FailedSHAShort != "deadbee" {
		t.Errorf("failed sha = %q", vm.Banner.FailedSHAShort)
	}
}

func TestBuildViewModel_BannerSuppressedBelowThreshold(t *testing.T) {
	t.Parallel()
	in, files, services, statuses := fixtureBuildViewModel()
	in.FailedCount = 2
	vm := buildViewModel(in, files, services, statuses, nil, status.OrphanScan{}, nil, fixtureNow, true)
	if vm.Banner != nil && vm.Banner.Active {
		t.Error("banner should be inactive when FailedCount < 3")
	}
}

func TestBuildViewModel_BannerSuppressedAfterExpiry(t *testing.T) {
	t.Parallel()
	in, files, services, statuses := fixtureBuildViewModel()
	in.FailedCount = 5
	in.FailedAt = fixtureNow.Add(-2 * time.Hour) // mirrors agent failure-gate expiry
	vm := buildViewModel(in, files, services, statuses, nil, status.OrphanScan{}, nil, fixtureNow, true)
	if vm.Banner != nil && vm.Banner.Active {
		t.Error("banner should be inactive when FailedAt > 1h ago")
	}
}

func TestBuildViewModel_Rows(t *testing.T) {
	t.Parallel()
	in, files, services, statuses := fixtureBuildViewModel()
	vm := buildViewModel(in, files, services, statuses, nil, status.OrphanScan{}, nil, fixtureNow, true)

	var web, manifest UnitRow
	for _, g := range vm.Groups {
		for _, r := range g.Rows {
			switch r.Basename {
			case "web.container":
				web = r
			case "manifest.yml":
				manifest = r
			}
		}
	}
	if web.Status.Token != "active" || web.SubState != "running" {
		t.Errorf("web row = %+v", web)
	}
	if manifest.Status.Class != "muted" {
		t.Errorf("manifest entry (no service) should be muted, got %+v", manifest.Status)
	}
}

func TestBuildViewModel_SystemdBasenameDerivation(t *testing.T) {
	t.Parallel()
	in, _, _, _ := fixtureBuildViewModel()
	in.FailedCount = 0

	files := map[string]state.ManagedFile{
		"/etc/systemd/system/custom.timer": {Hash: "sha256:tttt1111", Category: "systemd"},
	}
	statuses := map[string]status.UnitRuntimeStatus{
		"custom.timer": {ActiveState: "active", SubState: "running"},
	}
	vm := buildViewModel(in, files, nil, statuses, nil, status.OrphanScan{}, nil, fixtureNow, true)
	row := vm.Groups[0].Rows[0]
	if row.Status.Token != "active" {
		t.Errorf("systemd basename derivation failed (status): %+v", row)
	}
	if row.Service != "custom.timer" {
		t.Errorf("systemd basename should be written back to row.Service for template rendering, got %q", row.Service)
	}
}

func TestBuildViewModel_Dependencies(t *testing.T) {
	t.Parallel()
	in, files, services, statuses := fixtureBuildViewModel()
	vm := buildViewModel(in, files, services, statuses, map[string]status.UnitDependencies{
		"web.service": {
			Requires: []string{"internal-network.service"},
			After:    []string{"network-online.target"},
		},
	}, status.OrphanScan{}, nil, fixtureNow, true)

	var web UnitRow
	for _, g := range vm.Groups {
		for _, r := range g.Rows {
			if r.Service == "web.service" {
				web = r
			}
		}
	}
	if len(web.Dependencies) != 2 {
		t.Fatalf("dependency groups = %+v", web.Dependencies)
	}
	if web.Dependencies[0].Relation != "requires" {
		t.Errorf("first relation = %q", web.Dependencies[0].Relation)
	}
}
