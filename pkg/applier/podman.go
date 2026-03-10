package applier

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"

	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/pods"
	"github.com/containers/podman/v5/pkg/bindings/secrets"
	"github.com/containers/podman/v5/pkg/bindings/volumes"
)

// secretLabelKey and secretLabelValue are the Podman label applied to all picolet-managed secrets.
const (
	secretLabelKey   = "managed-by"
	secretLabelValue = "picolet"
)

// SocketPodmanClient implements PodmanClient using the Podman bindings over a Unix socket.
// The Podman bindings library embeds the socket connection into the context, so every
// binding call must use connCtx rather than the caller's request context.
//
//nolint:containedctx // Podman bindings require the connection context for every call
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

//nolint:contextcheck // must use connCtx; see SocketPodmanClient doc
func (c *SocketPodmanClient) SecretExists(_ context.Context, name string) (bool, error) {
	return secrets.Exists(c.connCtx, name)
}

//nolint:contextcheck // must use connCtx; see SocketPodmanClient doc
func (c *SocketPodmanClient) SecretCreate(_ context.Context, name string, data []byte, replace bool) error {
	opts := new(secrets.CreateOptions).
		WithName(name).
		WithReplace(replace).
		WithLabels(map[string]string{secretLabelKey: secretLabelValue})
	_, err := secrets.Create(c.connCtx, bytes.NewReader(data), opts)
	if err != nil {
		return fmt.Errorf("creating secret %s: %w", name, err)
	}
	return nil
}

//nolint:contextcheck // must use connCtx; see SocketPodmanClient doc
func (c *SocketPodmanClient) ListManagedSecrets(_ context.Context) ([]string, error) {
	opts := new(secrets.ListOptions).WithFilters(map[string][]string{
		"label": {secretLabelKey + "=" + secretLabelValue},
	})
	list, err := secrets.List(c.connCtx, opts)
	if err != nil {
		// Older Podman versions may not support label filtering on secrets.
		// Log and fall back to listing all secrets and filtering client-side.
		slog.Warn("listing secrets with label filter failed, falling back to full list", "error", err)
		list, err = secrets.List(c.connCtx, nil)
		if err != nil {
			return nil, fmt.Errorf("listing secrets: %w", err)
		}
	}
	names := make([]string, 0, len(list))
	for _, s := range list {
		if s.Spec.Labels[secretLabelKey] == secretLabelValue {
			names = append(names, s.Spec.Name)
		}
	}
	return names, nil
}

//nolint:contextcheck // must use connCtx; see SocketPodmanClient doc
func (c *SocketPodmanClient) SecretRemove(_ context.Context, name string) error {
	if err := secrets.Remove(c.connCtx, name); err != nil {
		code, _ := bindings.CheckResponseCode(err)
		if code == http.StatusNotFound {
			return nil // already gone
		}
		return fmt.Errorf("removing secret %s: %w", name, err)
	}
	return nil
}

//nolint:contextcheck // must use connCtx; see SocketPodmanClient doc
func (c *SocketPodmanClient) ContainerRemove(_ context.Context, nameOrID string, force bool) error {
	opts := new(containers.RemoveOptions).WithForce(force)
	_, err := containers.Remove(c.connCtx, nameOrID, opts)
	if err != nil {
		code, _ := bindings.CheckResponseCode(err)
		if code == http.StatusNotFound {
			return nil // already gone (auto-removed or cleaned up by ExecStop)
		}
		return fmt.Errorf("removing container %s: %w", nameOrID, err)
	}
	return nil
}

//nolint:contextcheck // must use connCtx; see SocketPodmanClient doc
func (c *SocketPodmanClient) RunHealthcheck(_ context.Context, container string) (bool, error) {
	result, err := containers.RunHealthCheck(c.connCtx, container, nil)
	if err != nil {
		return false, fmt.Errorf("healthcheck %s: %w", container, err)
	}
	return result.Status == define.HealthCheckHealthy, nil
}

//nolint:contextcheck // must use connCtx; see SocketPodmanClient doc
func (c *SocketPodmanClient) GetPodState(_ context.Context, pod string) (string, error) {
	report, err := pods.Inspect(c.connCtx, pod, nil)
	if err != nil {
		return "", fmt.Errorf("inspecting pod %s: %w", pod, err)
	}
	return report.State, nil
}

//nolint:contextcheck // must use connCtx; see SocketPodmanClient doc
func (c *SocketPodmanClient) VolumeRemove(_ context.Context, name string) error {
	if err := volumes.Remove(c.connCtx, name, nil); err != nil {
		code, _ := bindings.CheckResponseCode(err)
		if code == http.StatusNotFound {
			return nil // already gone
		}
		return fmt.Errorf("removing volume %s: %w", name, err)
	}
	return nil
}

// VolumeImportFiles imports files into a named Podman volume as a tar archive
// via the Podman API socket. Requires Podman 4.x+.
//
//nolint:contextcheck // must use connCtx; see SocketPodmanClient doc
func (c *SocketPodmanClient) VolumeImportFiles(_ context.Context, volumeName string, files map[string][]byte) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, path := range slices.Sorted(maps.Keys(files)) {
		content := files[path]
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(content))}); err != nil {
			return fmt.Errorf("writing tar header for %s: %w", path, err)
		}
		if _, err := tw.Write(content); err != nil {
			return fmt.Errorf("writing tar content for %s: %w", path, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("closing tar archive: %w", err)
	}
	if err := volumes.Import(c.connCtx, volumeName, &buf); err != nil {
		code, _ := bindings.CheckResponseCode(err)
		if code == http.StatusNotFound {
			return fmt.Errorf("volume %s does not exist: %w", volumeName, err)
		}
		return fmt.Errorf("importing files to volume %s: %w", volumeName, err)
	}
	return nil
}
