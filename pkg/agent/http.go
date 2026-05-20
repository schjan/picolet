package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/schjan/picolet/pkg/health"
	"github.com/schjan/picolet/pkg/metrics"
)

// healthFailureThreshold is the number of consecutive all-error health ticks
// before /health returns 503 to trigger a container restart.
const healthFailureThreshold = 3

func (a *Agent) newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !a.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"starting"}`))
			return
		}
		if !a.paused.Load() && a.consecutiveHealthFailures.Load() >= healthFailureThreshold {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"systemd_unreachable"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/webhook", webhookHandler(a.triggerReconcile, a.cfg.WebhookSecretPath))
	if a.routeRegistrar != nil {
		a.routeRegistrar.Register(mux)
	}
	return mux
}

func (a *Agent) startHTTP() (func(context.Context), error) {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", a.cfg.MetricsPort),
		Handler:           a.newMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return nil, fmt.Errorf("starting http listener on %s: %w", srv.Addr, err)
	}

	slog.Info("http server starting", "port", a.cfg.MetricsPort)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "error", err)
		}
	}()

	return func(ctx context.Context) {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("http server shutdown error", "error", err)
		}
	}, nil
}

// updateHealthFailures advances the consecutive-health-failure counter that
// gates /health. Lives in http.go alongside the reader so the cross-method
// coupling on the atomic is visible in one place.
func (a *Agent) updateHealthFailures(hr *health.CheckResult) {
	if hr.AllFailed() {
		a.consecutiveHealthFailures.Add(1)
	} else {
		a.consecutiveHealthFailures.Store(0)
	}
}
