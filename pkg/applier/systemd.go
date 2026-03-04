package applier

import (
	"context"
	"fmt"

	"github.com/coreos/go-systemd/v22/dbus"
)

// DBusSystemdManager implements SystemdManager using the systemd D-Bus API.
type DBusSystemdManager struct {
	conn *dbus.Conn
}

// NewDBusSystemdManager creates a new SystemdManager backed by D-Bus.
func NewDBusSystemdManager(ctx context.Context) (*DBusSystemdManager, error) {
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to systemd D-Bus: %w", err)
	}
	return &DBusSystemdManager{conn: conn}, nil
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
	return waitJobResult(ctx, ch, "starting", name)
}

func (m *DBusSystemdManager) RestartUnit(ctx context.Context, name string) error {
	ch := make(chan string, 1)
	_, err := m.conn.RestartUnitContext(ctx, name, "replace", ch)
	if err != nil {
		return fmt.Errorf("restarting %s: %w", name, err)
	}
	return waitJobResult(ctx, ch, "restarting", name)
}

// waitJobResult waits for a systemd job result or context cancellation.
func waitJobResult(ctx context.Context, ch <-chan string, verb, unit string) error {
	select {
	case result := <-ch:
		if result != "done" {
			return fmt.Errorf("%s %s: job result %q", verb, unit, result)
		}
		return nil
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
