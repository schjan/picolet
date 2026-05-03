// Package dashboard renders a single read-only HTML page describing the
// agent's current managed-unit state. It mounts on the existing metrics HTTP
// server alongside /metrics and /health.
package dashboard

import (
	"bytes"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
)

// Handler serves the dashboard index page and its static assets.
type Handler struct {
	store       *state.Store
	cfg         *agentcfg.Config
	version     string
	statusStore *status.Store
	tpl         *template.Template
	logger      *slog.Logger
	now         func() time.Time
}

// Option configures a Handler.
type Option func(*Handler)

// WithStatusStore provides live in-memory agent status to the dashboard.
func WithStatusStore(store *status.Store) Option {
	return func(h *Handler) { h.statusStore = store }
}

// NewHandler builds a dashboard handler. All dependencies may be nil for
// constrained test scenarios; nil store/cfg are tolerated and treated as empty.
func NewHandler(
	store *state.Store,
	cfg *agentcfg.Config,
	version string,
	logger *slog.Logger,
	opts ...Option,
) (*Handler, error) {
	if logger == nil {
		logger = slog.Default()
	}
	tpl, err := template.New("index.html").ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	h := &Handler{
		store:   store,
		cfg:     cfg,
		version: version,
		tpl:     tpl,
		logger:  logger,
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// SetNowForTest replaces the wall-clock for deterministic golden tests.
// Production code uses time.Now via NewHandler.
func (h *Handler) SetNowForTest(fn func() time.Time) { h.now = fn }

// Register attaches the dashboard's routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/", h.serveIndex)

	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		// Embedded FS uses a constant subdir — fs.Sub cannot fail outside of programmer error.
		panic(err)
	}
	mux.Handle("/static/", http.StripPrefix("/static/", staticCache(http.FileServerFS(staticFS))))
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// Set no-store before any potential error path so 5xx responses cannot be
	// cached by intermediaries during the 30s auto-refresh window.
	w.Header().Set("Cache-Control", "no-store")

	st, err := h.loadState()
	if err != nil {
		h.logger.Error("dashboard: state load failed", "err", err)
		http.Error(w, "state unavailable", http.StatusInternalServerError)
		return
	}
	snap := h.statusStore.Snapshot()

	vm := buildViewModel(
		h.buildHeaderInput(st, snap),
		st.ManagedFiles,
		st.ServiceNames,
		snap.Units,
		snap.Dependencies,
		snap.OrphanScan,
		snap.Events,
		h.now(),
		r.URL.Query().Get("refresh") != "0",
	)

	// Render into a buffer first so a template error never leaves the client
	// with a partial response — we only commit headers + bytes on success.
	var buf bytes.Buffer
	if err := h.tpl.ExecuteTemplate(&buf, "index.html", vm); err != nil {
		h.logger.Error("dashboard: template execute failed", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// loadState returns the persisted state, tolerating nil store as zero-state
// (test scenarios). Returns an error only on disk/decode failure.
func (h *Handler) loadState() (*state.State, error) {
	if h.store == nil {
		return &state.State{}, nil
	}
	loaded, err := h.store.Load()
	if err != nil {
		return nil, err
	}
	if loaded == nil {
		return &state.State{}, nil
	}
	return loaded, nil
}

func (h *Handler) buildHeaderInput(st *state.State, snap status.Snapshot) HeaderInput {
	var hostname string
	if h.cfg != nil {
		hostname = h.cfg.Hostname
	}
	return HeaderInput{
		Hostname:         hostname,
		Version:          h.version,
		AppliedSHA:       st.AppliedSHA,
		AppliedAt:        st.AppliedAt,
		VerifiedAt:       liveVerifiedAt(st, snap),
		PiType:           snap.Host.PiType,
		Features:         snap.Host.Features,
		ExternalHostname: snap.Host.ExternalHostname,
		FailedSHA:        st.FailedSHA,
		FailedCount:      st.FailedCount,
		FailedAt:         st.FailedAt,
	}
}

func liveVerifiedAt(st *state.State, snap status.Snapshot) time.Time {
	if !snap.VerifiedAt.IsZero() {
		return snap.VerifiedAt
	}
	return st.LastSuccessfulReconciliationAt
}

func staticCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		next.ServeHTTP(w, r)
	})
}
