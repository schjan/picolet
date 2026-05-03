package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/schjan/picolet/pkg/agentcfg"
)

// fakeRegistrar is a minimal RouteRegistrar. Avoiding an import of
// pkg/dashboard here is deliberate: it keeps pkg/agent UI-agnostic.
//
// It mirrors the production handler's catch-all "/" registration so the
// /health and /metrics assertions below actually exercise the shadowing
// risk — without a "/" route registered, ServeMux's longest-prefix match
// would always route /health and /metrics correctly regardless of bugs.
type fakeRegistrar struct{ called bool }

func (f *fakeRegistrar) Register(mux *http.ServeMux) {
	f.called = true
	mux.HandleFunc("/dashboard-marker", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestAgent_NewMux_RegistersRouteRegistrar(t *testing.T) {
	t.Parallel()
	fr := &fakeRegistrar{}
	a := newTestAgent(t, &agentcfg.Config{Hostname: "test"}, WithDashboard(fr))

	mux := a.newMux()
	if !fr.called {
		t.Fatal("WithDashboard registrar was not called by newMux")
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard-marker", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("registrar route returned %d, want 418", rec.Code)
	}

	// Existing routes still work — registering the dashboard's catch-all "/"
	// must not shadow /health or /metrics.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec2.Code != http.StatusOK && rec2.Code != http.StatusServiceUnavailable {
		t.Errorf("/health returned %d, want 200 or 503", rec2.Code)
	}

	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec3.Code != http.StatusOK {
		t.Errorf("/metrics returned %d, want 200", rec3.Code)
	}
}
