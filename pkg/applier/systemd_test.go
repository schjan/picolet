package applier

import (
	"context"
	"testing"

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
