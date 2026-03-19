package applier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

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
