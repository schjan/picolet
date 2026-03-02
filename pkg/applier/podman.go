package applier

import (
	"bytes"
	"context"
	"fmt"

	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/pods"
	"github.com/containers/podman/v5/pkg/bindings/secrets"
)

// connCtx stores the connection context returned by bindings.NewConnection.
// The Podman bindings library embeds the socket connection into the context;
// every binding call requires this specific context, not the caller's request context.
//
//nolint:containedctx // Podman bindings embed the connection in the context; storing it is required
type SocketPodmanClient struct {
	connCtx context.Context
}

// NewSocketPodmanClient creates a PodmanClient connected to the Podman socket.
// The returned client carries a connection context used by all binding calls.
func NewSocketPodmanClient(ctx context.Context, socketPath string) (*SocketPodmanClient, error) {
	connCtx, err := bindings.NewConnection(ctx, "unix:"+socketPath)
	if err != nil {
		return nil, fmt.Errorf("connecting to podman at %s: %w", socketPath, err)
	}
	return &SocketPodmanClient{connCtx: connCtx}, nil
}

//nolint:contextcheck // must use c.connCtx: Podman bindings require the connection context
func (c *SocketPodmanClient) SecretExists(_ context.Context, name string) (bool, error) {
	return secrets.Exists(c.connCtx, name)
}

//nolint:contextcheck // must use c.connCtx: Podman bindings require the connection context
func (c *SocketPodmanClient) SecretCreate(_ context.Context, name string, data []byte, replace bool) error {
	opts := new(secrets.CreateOptions).WithName(name).WithReplace(replace)
	_, err := secrets.Create(c.connCtx, bytes.NewReader(data), opts)
	if err != nil {
		return fmt.Errorf("creating secret %s: %w", name, err)
	}
	return nil
}

//nolint:contextcheck // must use c.connCtx: Podman bindings require the connection context
func (c *SocketPodmanClient) RunHealthcheck(_ context.Context, container string) (bool, error) {
	result, err := containers.RunHealthCheck(c.connCtx, container, nil)
	if err != nil {
		return false, fmt.Errorf("healthcheck %s: %w", container, err)
	}
	return result.Status == define.HealthCheckHealthy, nil
}

//nolint:contextcheck // must use c.connCtx: Podman bindings require the connection context
func (c *SocketPodmanClient) GetPodState(_ context.Context, pod string) (string, error) {
	report, err := pods.Inspect(c.connCtx, pod, nil)
	if err != nil {
		return "", fmt.Errorf("inspecting pod %s: %w", pod, err)
	}
	return report.State, nil
}
