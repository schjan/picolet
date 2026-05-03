// Package dashboard renders a single read-only HTML page describing the
// agent's current managed-unit state. It mounts on the existing metrics HTTP
// server alongside /metrics and /health.
package dashboard

import (
	"bytes"
	"context"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/state"
)

// statusCollectionTimeout caps the worst case if D-Bus stalls so the
// dashboard cannot hang the browser. ~30 sequential GetUnitStatus calls on
// a Pi take milliseconds; 2s is generous.
const statusCollectionTimeout = 2 * time.Second

// Handler serves the dashboard index page and its static assets.
type Handler struct {
	store   *state.Store
	systemd applier.SystemdManager
	cfg     *agentcfg.Config
	version string
	tpl     *template.Template
	logger  *slog.Logger
	now     func() time.Time
}

// NewHandler builds a dashboard handler. All dependencies may be nil for
// constrained test scenarios; nil store/systemd/cfg are tolerated and treated
// as empty.
func NewHandler(
	store *state.Store,
	sm applier.SystemdManager,
	cfg *agentcfg.Config,
	version string,
	logger *slog.Logger,
) (*Handler, error) {
	if logger == nil {
		logger = slog.Default()
	}
	tpl, err := template.New("index.html").ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Handler{
		store:   store,
		systemd: sm,
		cfg:     cfg,
		version: version,
		tpl:     tpl,
		logger:  logger,
		now:     time.Now,
	}, nil
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
	// http.Error preserves this header (it only resets Content-Type and
	// X-Content-Type-Options).
	w.Header().Set("Cache-Control", "no-store")

	// Tolerate nil store in tests; treat as zero-state.
	st := &state.State{}
	if h.store != nil {
		loaded, err := h.store.Load()
		if err != nil {
			h.logger.Error("dashboard: state load failed", "err", err)
			http.Error(w, "state unavailable", http.StatusInternalServerError)
			return
		}
		if loaded != nil {
			st = loaded
		}
	}

	statusCtx, cancel := context.WithTimeout(r.Context(), statusCollectionTimeout)
	defer cancel()
	statuses := h.collectStatuses(statusCtx, st.ManagedFiles, st.ServiceNames)

	var hostname string
	if h.cfg != nil {
		hostname = h.cfg.Hostname
	}

	in := HeaderInput{
		Hostname:    hostname,
		Version:     h.version,
		AppliedSHA:  st.AppliedSHA,
		AppliedAt:   st.AppliedAt,
		FailedSHA:   st.FailedSHA,
		FailedCount: st.FailedCount,
		FailedAt:    st.FailedAt,
	}
	vm := buildViewModel(in, st.ManagedFiles, st.ServiceNames, statuses, h.now())

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

// collectStatuses queries SystemdManager for each unique unit referenced by
// managed files. Unit-name resolution lives in unitNameFor. Units are
// queried in sorted order so that if the shared deadline truncates the loop
// on a slow host, the same prefix of units is queried each refresh — without
// this, randomized map iteration would cause rows to flip between real
// statuses and "unknown" between renders.
func (h *Handler) collectStatuses(
	ctx context.Context,
	files map[string]state.ManagedFile,
	services map[string]string,
) map[string]applier.UnitStatus {
	out := map[string]applier.UnitStatus{}
	if h.systemd == nil {
		return out
	}
	seen := map[string]bool{}
	units := make([]string, 0, len(files))
	for path, mf := range files {
		unit := unitNameFor(mf.Category, path, services[path])
		if unit == "" || seen[unit] {
			continue
		}
		seen[unit] = true
		units = append(units, unit)
	}
	slices.Sort(units)
	for _, unit := range units {
		st, err := h.systemd.GetUnitStatus(ctx, unit)
		if err != nil {
			h.logger.Debug("dashboard: GetUnitStatus failed", "unit", unit, "err", err)
			continue // absent → buildViewModel renders unknown
		}
		out[unit] = st
	}
	return out
}

func staticCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		next.ServeHTTP(w, r)
	})
}
