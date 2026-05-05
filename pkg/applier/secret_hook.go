package applier

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/schjan/picolet/pkg/config"
)

const (
	defaultReloadTimeout     = 5 * time.Second
	defaultReloadHealthDelay = 2 * time.Second
)

// SecretHookReloader executes secret-change runtime hooks.
type SecretHookReloader struct {
	systemd     SystemdManager
	podman      PodmanClient
	httpClient  *http.Client
	healthDelay time.Duration
}

// NewSecretHookReloader creates a hook runner with production defaults.
func NewSecretHookReloader(systemd SystemdManager, podman PodmanClient) *SecretHookReloader {
	return &SecretHookReloader{
		systemd:     systemd,
		podman:      podman,
		httpClient:  &http.Client{Timeout: defaultReloadTimeout},
		healthDelay: defaultReloadHealthDelay,
	}
}

// WithHTTPClient overrides the HTTP client used by reload hooks.
func (r *SecretHookReloader) WithHTTPClient(client *http.Client) *SecretHookReloader {
	if client != nil {
		r.httpClient = client
	}
	return r
}

// WithHealthDelay overrides the delay between HTTP reload and health check.
func (r *SecretHookReloader) WithHealthDelay(delay time.Duration) *SecretHookReloader {
	r.healthDelay = delay
	return r
}

// Run executes a hook. The bool return indicates whether the caller should
// restart hook.Unit after hook execution.
func (r *SecretHookReloader) Run(ctx context.Context, hook config.SecretHook, restartScheduled map[string]bool) (bool, error) {
	if hook.Action != config.HookActionRestart && restartScheduled[hook.Unit] {
		slog.Info("skipping secret hook, unit already scheduled for restart", "hook", hook.Name, "unit", hook.Unit)
		return false, nil
	}
	if hook.Action == config.HookActionRestart {
		slog.Info("secret hook scheduled restart", "hook", hook.Name, "unit", hook.Unit)
		return true, nil
	}
	active, err := r.unitActive(ctx, hook)
	if err != nil {
		return hook.OnFailure == config.HookOnFailureRestart, err
	}
	if !active {
		return false, nil
	}
	switch hook.Action {
	case config.HookActionHTTP:
		return r.runHTTP(ctx, hook)
	case config.HookActionSignal:
		return r.runSignal(ctx, hook)
	default:
		return false, fmt.Errorf("secret hook %s: unsupported action %q", hook.Name, hook.Action)
	}
}

func (r *SecretHookReloader) unitActive(ctx context.Context, hook config.SecretHook) (bool, error) {
	status, err := r.systemd.GetUnitStatus(ctx, hook.Unit)
	if err != nil {
		return false, fmt.Errorf("secret hook %s: checking unit %s: %w", hook.Name, hook.Unit, err)
	}
	switch status.ActiveState {
	case "active", "activating":
		return true, nil
	default:
		slog.Info("skipping secret hook, unit is not running",
			"hook", hook.Name,
			"unit", hook.Unit,
			"active_state", status.ActiveState,
			"sub_state", status.SubState,
		)
		return false, nil
	}
}

func (r *SecretHookReloader) runHTTP(ctx context.Context, hook config.SecretHook) (bool, error) {
	slog.Info("running HTTP secret hook", "hook", hook.Name, "method", hook.Method, "url", hook.URL, "unit", hook.Unit)
	if err := r.doHTTP(ctx, hook.Method, hook.URL); err != nil {
		return hook.OnFailure == config.HookOnFailureRestart, fmt.Errorf("secret hook %s: reload request: %w", hook.Name, err)
	}
	if hook.HealthURL != "" {
		if r.healthDelay > 0 {
			timer := time.NewTimer(r.healthDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return hook.OnFailure == config.HookOnFailureRestart, fmt.Errorf("secret hook %s: waiting for health check: %w", hook.Name, ctx.Err())
			}
		}
		if err := r.doHTTP(ctx, http.MethodGet, hook.HealthURL); err != nil {
			return hook.OnFailure == config.HookOnFailureRestart, fmt.Errorf("secret hook %s: health check: %w", hook.Name, err)
		}
	}
	return false, nil
}

func (r *SecretHookReloader) runSignal(ctx context.Context, hook config.SecretHook) (bool, error) {
	slog.Info("running signal secret hook", "hook", hook.Name, "container", hook.Container, "signal", hook.Signal, "unit", hook.Unit)
	if err := r.podman.ContainerKill(ctx, hook.Container, hook.Signal); err != nil {
		return hook.OnFailure == config.HookOnFailureRestart, fmt.Errorf("secret hook %s: signal container %s: %w", hook.Name, hook.Container, err)
	}
	return false, nil
}

func (r *SecretHookReloader) doHTTP(ctx context.Context, method, url string) error {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned status %d", method, url, resp.StatusCode)
	}
	return nil
}
