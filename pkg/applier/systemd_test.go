package applier

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitJobResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		result       string
		allowSkipped bool
		wantErr      bool
	}{
		{"done succeeds", "done", false, false},
		{"done succeeds with allowSkipped", "done", true, false},
		{"skipped succeeds when allowSkipped", "skipped", true, false},
		{"skipped fails when not allowSkipped", "skipped", false, true},
		{"failed returns error", "failed", false, true},
		{"failed returns error with allowSkipped", "failed", true, true},
		{"timeout returns error", "timeout", false, true},
		{"dependency returns error", "dependency", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := make(chan string, 1)
			ch <- tc.result
			err := waitJobResult(context.Background(), ch, "stopping", "test.service", tc.allowSkipped)
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
	err := waitJobResult(ctx, ch, "stopping", "test.service", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
