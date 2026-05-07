package applier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/schjan/picolet/pkg/config"
)

// ErrUnitNotActive is returned when a hook fires against a unit that is not
// in the active or activating state. The applier classifies this as a
// retryable failure so the next tick can attempt the reload once the unit
// comes up; without a sentinel the silent (false, nil) return path would
// drop the reload attempt and clear the pending entry forever.
var ErrUnitNotActive = errors.New("hook target unit not active")

// ErrHookSkipped is returned when a hook is intentionally not run because it
// is moot — the unit is already scheduled for restart this tick. The applier
// records a "skipped" outcome and leaves no error on the result.
var ErrHookSkipped = errors.New("hook skipped")

const (
	defaultReloadTimeout     = 5 * time.Second
	defaultReloadHealthDelay = 2 * time.Second
)

// HookReloader executes change-triggered runtime hooks.
type HookReloader struct {
	systemd     SystemdManager
	podman      PodmanClient
	httpClient  *http.Client
	healthDelay time.Duration
}

// NewHookReloader creates a hook runner with production defaults.
func NewHookReloader(systemd SystemdManager, podman PodmanClient) *HookReloader {
	return &HookReloader{
		systemd: systemd,
		podman:  podman,
		httpClient: &http.Client{
			Timeout:       defaultReloadTimeout,
			CheckRedirect: rejectRedirects,
		},
		healthDelay: defaultReloadHealthDelay,
	}
}

// rejectRedirects refuses any 3xx response. validateHookHTTPURL only checks the
// user-declared URL; without this guard a reload endpoint that returned 3xx
// would silently follow up to ten hops into arbitrary internal targets.
func rejectRedirects(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("hook redirected to %s: redirects are not permitted", req.URL.Redacted())
}

// WithHTTPClient overrides the HTTP client used by reload hooks. CheckRedirect
// is reinstalled on the supplied client so test-supplied clients still reject
// redirects unless the caller explicitly opts out.
func (r *HookReloader) WithHTTPClient(client *http.Client) *HookReloader {
	if client != nil {
		if client.CheckRedirect == nil {
			client.CheckRedirect = rejectRedirects
		}
		r.httpClient = client
	}
	return r
}

// WithHealthDelay overrides the delay between HTTP reload and health check.
func (r *HookReloader) WithHealthDelay(delay time.Duration) *HookReloader {
	r.healthDelay = delay
	return r
}

// Run executes a hook. The bool return indicates whether the caller should
// restart hook.Unit after hook execution. ErrUnitNotActive and ErrHookSkipped
// are recognized by dispatchHookResult and mapped to the retryable and
// skipped outcomes respectively.
func (r *HookReloader) Run(ctx context.Context, hook config.Hook, restartScheduled map[string]struct{}) (bool, error) {
	if _, scheduled := restartScheduled[hook.Unit]; scheduled && hook.Action != config.HookActionRestart {
		slog.Info("skipping hook, unit already scheduled for restart", "hook", hook.Name, "unit", hook.Unit)
		return false, ErrHookSkipped
	}
	if hook.Action == config.HookActionRestart {
		slog.Info("hook scheduled restart", "hook", hook.Name, "unit", hook.Unit)
		return true, nil
	}
	if err := r.unitActive(ctx, hook); err != nil {
		return hook.FallbackToRestart(), err
	}
	switch hook.Action {
	case config.HookActionHTTP:
		return r.runHTTP(ctx, hook)
	case config.HookActionSignal:
		return r.runSignal(ctx, hook)
	default:
		return false, fmt.Errorf("hook %s: unsupported action %q", hook.Name, hook.Action)
	}
}

// unitActive reports whether hook.Unit is in a state that can receive a
// reload. Returns ErrUnitNotActive (retryable) for inactive/failed/etc.
// states and a wrapped D-Bus error for transport failures.
func (r *HookReloader) unitActive(ctx context.Context, hook config.Hook) error {
	status, err := r.systemd.GetUnitStatus(ctx, hook.Unit)
	if err != nil {
		return fmt.Errorf("hook %s: checking unit %s: %w", hook.Name, hook.Unit, err)
	}
	switch status.ActiveState {
	case "active", "activating":
		return nil
	default:
		slog.Info("hook target unit not active, will retry",
			"hook", hook.Name,
			"unit", hook.Unit,
			"active_state", status.ActiveState,
			"sub_state", status.SubState,
		)
		return fmt.Errorf("hook %s: %w (state=%s/%s)", hook.Name, ErrUnitNotActive, status.ActiveState, status.SubState)
	}
}

func (r *HookReloader) runHTTP(ctx context.Context, hook config.Hook) (bool, error) {
	slog.Info("running HTTP hook", "hook", hook.Name, "method", hook.Method, "url", hook.URL, "unit", hook.Unit)
	if err := r.doHTTP(ctx, hook.Method, hook.URL); err != nil {
		return hook.FallbackToRestart(), fmt.Errorf("hook %s: reload request: %w", hook.Name, err)
	}
	if hook.HealthURL != "" {
		if r.healthDelay > 0 {
			timer := time.NewTimer(r.healthDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return hook.FallbackToRestart(), fmt.Errorf("hook %s: waiting for health check: %w", hook.Name, ctx.Err())
			}
		}
		if err := r.doHTTP(ctx, http.MethodGet, hook.HealthURL); err != nil {
			return hook.FallbackToRestart(), fmt.Errorf("hook %s: health check: %w", hook.Name, err)
		}
	}
	return false, nil
}

func (r *HookReloader) runSignal(ctx context.Context, hook config.Hook) (bool, error) {
	slog.Info("running signal hook", "hook", hook.Name, "container", hook.Container, "signal", hook.Signal, "unit", hook.Unit)
	if err := r.podman.ContainerKill(ctx, hook.Container, hook.Signal); err != nil {
		return hook.FallbackToRestart(), fmt.Errorf("hook %s: signal container %s: %w", hook.Name, hook.Container, err)
	}
	return false, nil
}

func (r *HookReloader) doHTTP(ctx context.Context, method, url string) error {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("performing %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned status %d", method, url, resp.StatusCode)
	}
	return nil
}
