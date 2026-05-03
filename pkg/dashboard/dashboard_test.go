package dashboard_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/mock"

	mockapplier "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/dashboard"
	"github.com/schjan/picolet/pkg/state"
)

var fixedNow = time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

func newTestStore(t *testing.T, st state.State) *state.Store {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return state.NewStore(p)
}

func TestHandler_ServesPlaceholder(t *testing.T) {
	t.Parallel()

	h, err := dashboard.NewHandler(nil, nil, nil, "0.0.0-test", nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("content-type = %q, want text/html...", got)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "picolet") {
		t.Fatalf("body missing 'picolet'; got %q", body)
	}
}

func TestHandler_404OnNonRoot(t *testing.T) {
	t.Parallel()
	h, _ := dashboard.NewHandler(nil, nil, nil, "0.0.0-test", nil)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/totally-bogus", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandler_HTMLCacheControl(t *testing.T) {
	t.Parallel()
	h, _ := dashboard.NewHandler(nil, nil, nil, "0.0.0", nil)
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("HTML Cache-Control = %q, want no-store", got)
	}
}

func TestHandler_StaticAssetServed(t *testing.T) {
	t.Parallel()
	h, _ := dashboard.NewHandler(nil, nil, nil, "0.0.0", nil)
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/picolet.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/css") {
		t.Errorf("content-type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=86400" {
		t.Errorf("static Cache-Control = %q", got)
	}
}

func TestServeIndex_RendersUnitsAndStatuses(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{Hostname: "pi-edge-01"}
	store := newTestStore(t, state.State{
		AppliedSHA: "abc1234abc1234",
		AppliedAt:  time.Now().Add(-2 * time.Minute),
		ManagedFiles: map[string]state.ManagedFile{
			"/p/web.container": {Hash: "sha256:h-web-aaaa", Category: "container"},
		},
		ServiceNames: map[string]string{"/p/web.container": "web.service"},
	})

	sm := mockapplier.NewMockSystemdManager(t)
	sm.EXPECT().GetUnitStatus(mock.Anything, "web.service").
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)

	h, err := dashboard.NewHandler(store, sm, cfg, "0.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"pi-edge-01", "abc1234", "web.container", "active", "running", "█"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestServeIndex_FailureGateBanner(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{Hostname: "pi-edge-01"}
	store := newTestStore(t, state.State{
		FailedSHA: "deadbeefcafe", FailedCount: 4, FailedAt: time.Now(),
	})
	sm := mockapplier.NewMockSystemdManager(t)
	h, _ := dashboard.NewHandler(store, sm, cfg, "0.0.0", nil)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "deadbee") {
		t.Errorf("banner missing failed-sha short form; body=%s", body)
	}
	if !strings.Contains(body, "recent repeated failure") {
		t.Errorf("banner missing softened phrasing; body=%s", body)
	}
}

func TestServeIndex_SystemdCategoryDerivesUnitName(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{Hostname: "pi-edge-01"}
	store := newTestStore(t, state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/systemd/system/custom.timer": {Hash: "sha256:tttt", Category: "systemd"},
		},
		// No ServiceNames mapping — handler must fall back to filepath.Base(path).
	})

	sm := mockapplier.NewMockSystemdManager(t)
	sm.EXPECT().GetUnitStatus(mock.Anything, "custom.timer").
		Return(applier.UnitStatus{ActiveState: "active", SubState: "waiting"}, nil)

	h, _ := dashboard.NewHandler(store, sm, cfg, "0.0.0", nil)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"custom.timer", "active", "waiting", "█"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestServeIndex_NoStoreOnLoadError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := os.WriteFile(p, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(p)

	cfg := &agentcfg.Config{Hostname: "pi-edge-01"}
	sm := mockapplier.NewMockSystemdManager(t)
	h, _ := dashboard.NewHandler(store, sm, cfg, "0.0.0", nil)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control on 500 = %q, want no-store (errors must not be cached during 30s auto-refresh window)", got)
	}
}

func TestServeIndex_PerUnitErrorIsTolerant(t *testing.T) {
	t.Parallel()
	cfg := &agentcfg.Config{Hostname: "pi-edge-01"}
	store := newTestStore(t, state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/p/web.container": {Hash: "sha256:h-web", Category: "container"},
		},
		ServiceNames: map[string]string{"/p/web.container": "web.service"},
	})
	sm := mockapplier.NewMockSystemdManager(t)
	sm.EXPECT().GetUnitStatus(mock.Anything, "web.service").
		Return(applier.UnitStatus{}, errors.New("dbus borked"))

	h, _ := dashboard.NewHandler(store, sm, cfg, "0.0.0", nil)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (must not 500 on per-unit failure)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown") {
		t.Errorf("expected 'unknown' status fallback")
	}
}

func TestServeIndex_Goldie(t *testing.T) {
	t.Parallel()

	cfg := &agentcfg.Config{Hostname: "pi-edge-01"}
	store := newTestStore(t, state.State{
		AppliedSHA: "0123456789abcdef",
		AppliedAt:  fixedNow.Add(-5 * time.Minute),
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/picolet/web.container": {Hash: "sha256:aaaaaaaa1111", Category: "container"},
			"/etc/containers/systemd/picolet/db.container":  {Hash: "sha256:bbbbbbbb2222", Category: "container"},
			"/etc/containers/systemd/picolet/lan.network":   {Hash: "sha256:cccccccc3333", Category: "network"},
			"/etc/containers/systemd/picolet/data.volume":   {Hash: "sha256:dddddddd4444", Category: "volume"},
			"/etc/containers/systemd/picolet/app.yml":       {Hash: "sha256:eeeeeeee5555", Category: "manifest"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/picolet/web.container": "web.service",
			"/etc/containers/systemd/picolet/db.container":  "db.service",
			"/etc/containers/systemd/picolet/lan.network":   "lan-network.service",
			"/etc/containers/systemd/picolet/data.volume":   "data-volume.service",
			// app.yml (category=manifest) intentionally has no service mapping → renders muted.
		},
	})

	sm := mockapplier.NewMockSystemdManager(t)
	sm.EXPECT().GetUnitStatus(mock.Anything, "web.service").
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	sm.EXPECT().GetUnitStatus(mock.Anything, "db.service").
		Return(applier.UnitStatus{ActiveState: "failed", SubState: "dead"}, nil)
	sm.EXPECT().GetUnitStatus(mock.Anything, "lan-network.service").
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)
	sm.EXPECT().GetUnitStatus(mock.Anything, "data-volume.service").
		Return(applier.UnitStatus{ActiveState: "active", SubState: "running"}, nil)

	h, err := dashboard.NewHandler(store, sm, cfg, "0.7.2-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	h.SetNowForTest(func() time.Time { return fixedNow })

	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	g := goldie.New(t,
		goldie.WithFixtureDir("testdata/golden"),
		goldie.WithNameSuffix(".golden.html"),
	)
	g.Assert(t, "index", rec.Body.Bytes())
}
