package applier

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// systemd job result strings as documented in go-systemd dbus/methods.go:97-102.
// No library constants exist; these values are sent over D-Bus.
const (
	systemdJobDone    = "done"
	systemdJobSkipped = "skipped"
)

// DBusSystemdManager implements SystemdManager using the systemd D-Bus API.
type DBusSystemdManager struct {
	conn *dbus.Conn
}

// NewDBusSystemdManager creates a new SystemdManager backed by D-Bus.
// When rootless is true, it connects directly to the user systemd private socket,
// bypassing the D-Bus session bus to avoid EXTERNAL auth failures in user namespaces.
func NewDBusSystemdManager(ctx context.Context, rootless bool) (*DBusSystemdManager, error) {
	var conn *dbus.Conn
	var err error
	busType := "system"
	if rootless {
		conn, err = newUserSystemdConnectionContext(ctx)
		busType = "user-systemd"
	} else {
		conn, err = dbus.NewSystemConnectionContext(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to %s D-Bus: %w", busType, err)
	}
	return &DBusSystemdManager{conn: conn}, nil
}

// newUserSystemdConnectionContext connects directly to the user systemd instance via
// its private socket, bypassing the D-Bus session bus. Required for rootless containers
// where D-Bus EXTERNAL auth fails due to user namespace UID remapping.
func newUserSystemdConnectionContext(ctx context.Context) (*dbus.Conn, error) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		return nil, fmt.Errorf("XDG_RUNTIME_DIR not set")
	}
	// Pre-compute outside the closure: dbus.NewConnection calls it twice
	// (once for sysconn, once for sigconn).
	socketPath := "unix:path=" + filepath.Join(runtimeDir, "systemd", "private")

	return dbus.NewConnection(func() (*godbus.Conn, error) {
		conn, err := godbus.Dial(socketPath, godbus.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		// Empty string: server uses SO_PEERCRED for identity (correct for user-namespace
		// containers where os.Getuid()=0 but host SO_PEERCRED UID differs). godbus v5
		// discards the resp from AuthExternal.FirstData() regardless, so this is also
		// the more explicit form of "authenticate via socket credentials only."
		if err = conn.Auth([]godbus.Auth{godbus.AuthExternal("")}); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	})
}

// Close closes the D-Bus connection.
func (m *DBusSystemdManager) Close() {
	m.conn.Close()
}

func (m *DBusSystemdManager) DaemonReload(ctx context.Context) error {
	return m.conn.ReloadContext(ctx)
}

func (m *DBusSystemdManager) StartUnit(ctx context.Context, name string) error {
	ch := make(chan string, 1)
	_, err := m.conn.StartUnitContext(ctx, name, "replace", ch)
	if err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}
	return waitJobDone(ctx, ch, "starting", name)
}

func (m *DBusSystemdManager) StopUnit(ctx context.Context, name string) error {
	ch := make(chan string, 1)
	_, err := m.conn.StopUnitContext(ctx, name, "replace", ch)
	if err != nil {
		return fmt.Errorf("stopping %s: %w", name, err)
	}
	return waitJobDoneOrSkipped(ctx, ch, "stopping", name)
}

func (m *DBusSystemdManager) RestartUnit(ctx context.Context, name string) error {
	ch := make(chan string, 1)
	_, err := m.conn.RestartUnitContext(ctx, name, "replace", ch)
	if err != nil {
		return fmt.Errorf("restarting %s: %w", name, err)
	}
	return waitJobDone(ctx, ch, "restarting", name)
}

// waitJobDone waits for a systemd job to complete with "done".
func waitJobDone(ctx context.Context, ch <-chan string, verb, unit string) error {
	return waitJobResult(ctx, ch, verb, unit, systemdJobDone)
}

// waitJobDoneOrSkipped waits for a systemd job to complete with "done" or "skipped".
// Stop operations return "skipped" when the unit is already inactive, which is a
// valid outcome.
func waitJobDoneOrSkipped(ctx context.Context, ch <-chan string, verb, unit string) error {
	return waitJobResult(ctx, ch, verb, unit, systemdJobDone, systemdJobSkipped)
}

// waitJobResult waits for a systemd job result or context cancellation.
// The result must match one of the accepted values; any other result is an error.
func waitJobResult(ctx context.Context, ch <-chan string, verb, unit string, accepted ...string) error {
	select {
	case result := <-ch:
		if slices.Contains(accepted, result) {
			return nil
		}
		return fmt.Errorf("%s %s: job result %q", verb, unit, result)
	case <-ctx.Done():
		return fmt.Errorf("%s %s: %w", verb, unit, ctx.Err())
	}
}

func (m *DBusSystemdManager) GetUnitState(ctx context.Context, name string) (string, error) {
	props, err := m.conn.GetUnitPropertiesContext(ctx, name)
	if err != nil {
		return "", fmt.Errorf("getting state of %s: %w", name, err)
	}
	state, ok := props["ActiveState"].(string)
	if !ok {
		return "", fmt.Errorf("ActiveState not a string for %s", name)
	}
	return state, nil
}

func (m *DBusSystemdManager) IsActive(ctx context.Context, name string) (bool, error) {
	state, err := m.GetUnitState(ctx, name)
	if err != nil {
		return false, err
	}
	return state == "active" || state == "activating", nil
}
