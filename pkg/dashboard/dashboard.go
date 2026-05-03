// Package dashboard renders a single read-only HTML page describing the
// agent's current managed-unit state. It mounts on the existing metrics HTTP
// server alongside /metrics and /health.
package dashboard

import (
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/state"
)

// Handler serves the dashboard index page and its static assets.
type Handler struct {
	store   *state.Store
	systemd applier.SystemdManager
	cfg     *agentcfg.Config
	version string
	tpl     *template.Template
	logger  *slog.Logger
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
	}, nil
}

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
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tpl.ExecuteTemplate(w, "index.html", map[string]string{"Version": h.version}); err != nil {
		h.logger.Error("dashboard render failed", "err", err)
	}
}

func staticCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		next.ServeHTTP(w, r)
	})
}
