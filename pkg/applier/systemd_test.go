package applier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"testing"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewUserSystemdConnectionContext_NoRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	_, err := newUserSystemdConnectionContext(context.Background())
	require.ErrorContains(t, err, "XDG_RUNTIME_DIR not set")
}

func TestNewUserSystemdConnectionContext_SocketPath(t *testing.T) {
	// Can't connect to a real socket in unit tests; verify dial fails
	// with socket-not-found, NOT with XDG_RUNTIME_DIR error.
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent-runtime-dir")
	_, err := newUserSystemdConnectionContext(context.Background())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "XDG_RUNTIME_DIR not set")
}

func TestWaitJobDone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		result  string
		wantErr bool
	}{
		{"done succeeds", "done", false},
		{"skipped fails", "skipped", true},
		{"failed returns error", "failed", true},
		{"timeout returns error", "timeout", true},
		{"dependency returns error", "dependency", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := make(chan string, 1)
			ch <- tc.result
			err := waitJobDone(context.Background(), ch, "starting", "test.service")
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.result)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestWaitJobDoneOrSkipped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		result  string
		wantErr bool
	}{
		{"done succeeds", "done", false},
		{"skipped succeeds", "skipped", false},
		{"failed returns error", "failed", true},
		{"timeout returns error", "timeout", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := make(chan string, 1)
			ch <- tc.result
			err := waitJobDoneOrSkipped(context.Background(), ch, "stopping", "test.service")
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.result)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestWaitJobResultContextCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan string) // unbuffered, never sends
	err := waitJobDone(ctx, ch, "stopping", "test.service")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWaitJobResultTimeout(t *testing.T) {
	t.Parallel()
	// Override jobTimeout for test speed — not possible with a const, so we test
	// that an empty channel with a cancelled-free context eventually times out.
	// Since jobTimeout is 30s this test verifies the timeout path exists by using
	// a context that expires before the job timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	ch := make(chan string) // never sends
	err := waitJobDone(ctx, ch, "starting", "test.service")
	require.Error(t, err)
	// Either context deadline or job timeout fires — both are valid
	assert.Contains(t, err.Error(), "starting test.service")
}

func TestReconnectCooldown(t *testing.T) {
	t.Parallel()

	t.Run("within cooldown returns cached error", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		cachedErr := fmt.Errorf("D-Bus reconnect: connecting to user-systemd D-Bus: connection refused")

		m := &DBusSystemdManager{
			rootless:             true,
			lastReconnectAttempt: now.Add(-10 * time.Second), // 10s ago, within 30s cooldown
			lastReconnectErr:     cachedErr,
			nowFn:                func() time.Time { return now },
		}
		// conn is nil — the pointer-equality guard (m.conn == conn) will match,
		// so the cooldown check runs.

		err := m.withReconnect(context.Background(), func(_ *dbus.Conn) error {
			return godbus.ErrClosed // triggers reconnect path
		})
		// Should return cached error, not attempt reconnect (which would panic on nil conn.Close)
		require.Error(t, err)
		assert.Equal(t, cachedErr, err)
	})

	t.Run("expired cooldown retries reconnect", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		cachedErr := fmt.Errorf("D-Bus reconnect: connection refused")

		m := &DBusSystemdManager{
			rootless:             true,
			lastReconnectAttempt: now.Add(-60 * time.Second), // 60s ago, past 30s cooldown
			lastReconnectErr:     cachedErr,
			nowFn:                func() time.Time { return now },
		}

		// reconnect will fail (nil conn → panic), but we can verify the cooldown was bypassed
		// by checking that it doesn't return the cached error.
		// We can't test actual reconnect without a real D-Bus, so verify the code path
		// by catching the panic from conn.Close() on nil conn.
		assert.Panics(t, func() {
			_ = m.withReconnect(context.Background(), func(_ *dbus.Conn) error {
				return godbus.ErrClosed
			})
		}, "should attempt reconnect (and panic on nil conn) when cooldown is expired")
	})

	t.Run("nil lastReconnectErr bypasses cooldown", func(t *testing.T) {
		t.Parallel()
		now := time.Now()

		m := &DBusSystemdManager{
			rootless:             true,
			lastReconnectAttempt: now.Add(-5 * time.Second), // recent, but err is nil
			lastReconnectErr:     nil,                       // no cached error → don't suppress
			nowFn:                func() time.Time { return now },
		}

		// Should attempt reconnect (not return nil), hitting the nil conn panic.
		assert.Panics(t, func() {
			_ = m.withReconnect(context.Background(), func(_ *dbus.Conn) error {
				return godbus.ErrClosed
			})
		}, "should attempt reconnect when lastReconnectErr is nil regardless of time")
	})
}

func TestIsConnectionDead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"godbus ErrClosed", godbus.ErrClosed, true},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"net.ErrClosed", net.ErrClosed, true},
		{"wrapped godbus ErrClosed", fmt.Errorf("dbus call: %w", godbus.ErrClosed), true},
		{"wrapped io.EOF", fmt.Errorf("read: %w", io.EOF), true},
		{"wrapped net.ErrClosed", fmt.Errorf("socket: %w", net.ErrClosed), true},
		{"double wrapped", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", godbus.ErrClosed)), true},
		{"nil error", nil, false},
		{"unrelated error", errors.New("something else"), false},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isConnectionDead(tc.err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The run timestamps feed metrics that alert on a job having stopped succeeding,
// so a decode that silently yields zero would delete the series rather than
// surface the bug. usecProp therefore fails closed on anything it cannot read and
// maps only systemd's own "never happened" encodings to the zero time.
func TestUsecProp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		props   map[string]any
		want    time.Time
		wantErr string
	}{
		{
			"realtime microseconds",
			map[string]any{"InactiveEnterTimestamp": uint64(1_800_000_000_500_000)},
			time.UnixMicro(1_800_000_000_500_000).UTC(), "",
		},
		{
			"zero is never happened, not the epoch",
			map[string]any{"InactiveEnterTimestamp": uint64(0)},
			time.Time{},
			"",
		},
		{
			// Same guard rejects anything above MaxInt64, which no realtime stamp reaches.
			"USEC_INFINITY is unset, not year 586524",
			map[string]any{"InactiveEnterTimestamp": uint64(math.MaxUint64)},
			time.Time{},
			"",
		},
		{
			"missing property fails closed",
			map[string]any{},
			time.Time{},
			"job.service has no InactiveEnterTimestamp property",
		},
		{
			"wrong type fails closed",
			map[string]any{"InactiveEnterTimestamp": int64(17)},
			time.Time{},
			"InactiveEnterTimestamp of job.service is not a uint64",
		},
		{
			"nil value fails closed",
			map[string]any{"InactiveEnterTimestamp": nil},
			time.Time{},
			"InactiveEnterTimestamp of job.service is not a uint64",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := usecProp(tc.props, "InactiveEnterTimestamp", "job.service")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.True(t, got.Equal(tc.want), "got %v, want %v", got, tc.want)
		})
	}
}

func TestUnitStatusFromProps(t *testing.T) {
	t.Parallel()
	// Untyped constants: both the uint64 property values and the int64 time.UnixMicro
	// arguments below are constant conversions, so no runtime narrowing happens.
	const (
		startedUSec  = 1_800_000_000_000_000
		finishedUSec = startedUSec + 30_000_000
	)

	t.Run("decodes the unit interface", func(t *testing.T) {
		t.Parallel()
		status, err := unitStatusFromProps("job.service", map[string]any{
			"ActiveState":            "inactive",
			"SubState":               "dead",
			"UnitFileState":          "static",
			"TriggeredBy":            []string{"job.timer"},
			"InactiveExitTimestamp":  uint64(startedUSec),
			"InactiveEnterTimestamp": uint64(finishedUSec),
		})
		require.NoError(t, err)
		assert.Equal(t, "inactive", status.ActiveState)
		assert.Equal(t, []string{"job.timer"}, status.TriggeredBy)
		assert.True(t, status.LastRunStartedAt.Equal(time.UnixMicro(startedUSec).UTC()))
		assert.True(t, status.LastRunFinishedAt.Equal(time.UnixMicro(finishedUSec).UTC()))
		// Scoped extras belong to the per-interface reads, not this decode.
		assert.Empty(t, status.ServiceType)
		assert.Empty(t, status.Result)
		assert.Zero(t, status.LastTriggerAt)
	})

	t.Run("a unit that never ran reports no run times", func(t *testing.T) {
		t.Parallel()
		status, err := unitStatusFromProps("job.timer", map[string]any{
			"ActiveState":            "active",
			"SubState":               "waiting",
			"InactiveExitTimestamp":  uint64(0),
			"InactiveEnterTimestamp": uint64(0),
		})
		require.NoError(t, err)
		assert.Zero(t, status.LastRunStartedAt)
		assert.Zero(t, status.LastRunFinishedAt)
	})

	t.Run("a missing run timestamp fails the whole read", func(t *testing.T) {
		t.Parallel()
		_, err := unitStatusFromProps("job.service", map[string]any{
			"ActiveState":           "active",
			"SubState":              "running",
			"InactiveExitTimestamp": uint64(0),
		})
		require.ErrorContains(t, err, "InactiveEnterTimestamp")
	})

	t.Run("a non-string ActiveState fails the whole read", func(t *testing.T) {
		t.Parallel()
		_, err := unitStatusFromProps("job.service", map[string]any{
			"ActiveState":            uint64(1),
			"InactiveExitTimestamp":  uint64(0),
			"InactiveEnterTimestamp": uint64(0),
		})
		require.ErrorContains(t, err, "ActiveState not a string")
	})
}
