package applier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// systemd job result strings as documented in go-systemd dbus/methods.go:97-102.
// No library constants exist; these values are sent over D-Bus.
const (
	systemdJobDone    = "done"
	systemdJobSkipped = "skipped"
)

// jobTimeout is the maximum time to wait for a systemd job signal before giving up.
// If D-Bus dies between a successful unit operation and the job signal, the channel
// hangs forever. This timeout lets the tick fail so the next tick can reconnect.
const jobTimeout = 30 * time.Second

// DBusSystemdManager implements SystemdManager using the systemd D-Bus API.
type DBusSystemdManager struct {
	mu       sync.Mutex
	conn     *dbus.Conn
	rootless bool
}

// NewDBusSystemdManager creates a new SystemdManager backed by D-Bus.
// When rootless is true, it connects directly to the user systemd private socket,
// bypassing the D-Bus session bus to avoid EXTERNAL auth failures in user namespaces.
func NewDBusSystemdManager(ctx context.Context, rootless bool) (*DBusSystemdManager, error) {
	conn, err := connect(ctx, rootless)
	if err != nil {
		return nil, err
	}
	return &DBusSystemdManager{conn: conn, rootless: rootless}, nil
}

// connect creates a new D-Bus connection based on the rootless flag.
func connect(ctx context.Context, rootless bool) (*dbus.Conn, error) {
	if rootless {
		conn, err := newUserSystemdConnectionContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("connecting to user-systemd D-Bus: %w", err)
		}
		return conn, nil
	}
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to system D-Bus: %w", err)
	}
	return conn, nil
}

// reconnect closes the old connection and establishes a new one.
// Must be called with m.mu held.
func (m *DBusSystemdManager) reconnect(ctx context.Context) error {
	m.conn.Close()
	conn, err := connect(ctx, m.rootless)
	if err != nil {
		return fmt.Errorf("D-Bus reconnect: %w", err)
	}
	m.conn = conn
	return nil
}

// withReconnect runs fn with the current connection. If fn returns a dead-connection
// error and the context is still active, it reconnects once and retries.
func (m *DBusSystemdManager) withReconnect(ctx context.Context, fn func(*dbus.Conn) error) error {
	m.mu.Lock()
	conn := m.conn
	m.mu.Unlock()

	err := fn(conn)
	if err == nil || !isConnectionDead(err) {
		return err
	}

	// Don't reconnect during shutdown — the new connection would be immediately killed.
	if ctx.Err() != nil {
		return err
	}

	m.mu.Lock()
	// Only reconnect if no other goroutine already did (pointer-equality guard).
	if m.conn == conn {
		if err := m.reconnect(ctx); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	conn = m.conn
	m.mu.Unlock()

	return fn(conn)
}

// isConnectionDead returns true if the error indicates a dead D-Bus connection
// that warrants a reconnection attempt.
func isConnectionDead(err error) bool {
	return errors.Is(err, godbus.ErrClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed)
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
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conn.Close()
}

func (m *DBusSystemdManager) DaemonReload(ctx context.Context) error {
	return m.withReconnect(ctx, func(c *dbus.Conn) error {
		return c.ReloadContext(ctx)
	})
}

func (m *DBusSystemdManager) StartUnit(ctx context.Context, name string) error {
	return m.withReconnect(ctx, func(c *dbus.Conn) error {
		ch := make(chan string, 1)
		if _, err := c.StartUnitContext(ctx, name, "replace", ch); err != nil {
			return fmt.Errorf("starting %s: %w", name, err)
		}
		return waitJobDone(ctx, ch, "starting", name)
	})
}

func (m *DBusSystemdManager) StopUnit(ctx context.Context, name string) error {
	return m.withReconnect(ctx, func(c *dbus.Conn) error {
		ch := make(chan string, 1)
		if _, err := c.StopUnitContext(ctx, name, "replace", ch); err != nil {
			return fmt.Errorf("stopping %s: %w", name, err)
		}
		return waitJobDoneOrSkipped(ctx, ch, "stopping", name)
	})
}

func (m *DBusSystemdManager) RestartUnit(ctx context.Context, name string) error {
	return m.withReconnect(ctx, func(c *dbus.Conn) error {
		ch := make(chan string, 1)
		if _, err := c.RestartUnitContext(ctx, name, "replace", ch); err != nil {
			return fmt.Errorf("restarting %s: %w", name, err)
		}
		return waitJobDone(ctx, ch, "restarting", name)
	})
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

// waitJobResult waits for a systemd job result, context cancellation, or timeout.
// The result must match one of the accepted values; any other result is an error.
// The timeout prevents hanging forever if D-Bus dies between the unit operation
// and the job signal arriving (the channel is caller-owned and won't be closed).
func waitJobResult(ctx context.Context, ch <-chan string, verb, unit string, accepted ...string) error {
	select {
	case result := <-ch:
		if slices.Contains(accepted, result) {
			return nil
		}
		return fmt.Errorf("%s %s: job result %q", verb, unit, result)
	case <-time.After(jobTimeout):
		return fmt.Errorf("%s %s: timeout waiting for job result", verb, unit)
	case <-ctx.Done():
		return fmt.Errorf("%s %s: %w", verb, unit, ctx.Err())
	}
}

func (m *DBusSystemdManager) GetUnitStatus(ctx context.Context, name string) (UnitStatus, error) {
	var status UnitStatus
	err := m.withReconnect(ctx, func(c *dbus.Conn) error {
		props, err := c.GetUnitPropertiesContext(ctx, name)
		if err != nil {
			return fmt.Errorf("getting status of %s: %w", name, err)
		}
		activeState := stringProp(props, "ActiveState")
		if activeState == "" {
			return fmt.Errorf("ActiveState not a string for %s", name)
		}
		status = UnitStatus{
			ActiveState: activeState,
			SubState:    stringProp(props, "SubState"),
		}
		return nil
	})
	return status, err
}

func stringProp(props map[string]any, key string) string {
	s, _ := props[key].(string)
	return s
}
