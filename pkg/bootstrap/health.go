package bootstrap

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxHealthProbeTimeout = 5 * time.Second

var healthHTTPClient = http.DefaultClient

func WaitForHealth(ctx context.Context, port int, healthPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		probeCtx, cancel := context.WithTimeout(ctx, min(maxHealthProbeTimeout, remaining))
		err := probeOnce(probeCtx, port, healthPath)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		sleep := min(2*time.Second, time.Until(deadline))
		if sleep <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
	}
	return fmt.Errorf("picolet did not report healthy within %s: %w", timeout, lastErr)
}

func probeOnce(ctx context.Context, port int, healthPath string) error {
	if !strings.HasPrefix(healthPath, "/") {
		healthPath = "/" + healthPath
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://localhost:%d%s", port, healthPath), nil)
	if err != nil {
		return err
	}
	resp, err := healthHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
