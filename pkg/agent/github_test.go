package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	agentmocks "github.com/schjan/picolet/mocks/agent"
	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/metrics"
)

func TestCreateDeploymentContinuesWhenPendingStatusFails(t *testing.T) {
	t.Parallel()
	metrics.Register(nil)

	cfg := &agentcfg.Config{Hostname: "test-host", RepoURL: "https://example.com/repo.git"}
	reporter := agentmocks.NewMockDeploymentReporter(t)
	reporter.EXPECT().CreateDeployment(mock.Anything, "abc123").Return(int64(42), errors.New("pending status failed"))
	reporter.EXPECT().ReportInProgress(mock.Anything, int64(42)).Return(nil)

	a := newTestAgent(t, cfg, WithDeploymentReporter(reporter))
	got := a.createDeployment(context.Background(), "abc123")
	assert.Equal(t, int64(42), got)
}

func TestReportDeploymentResultUsesErrorForRollback(t *testing.T) {
	t.Parallel()
	metrics.Register(nil)

	beforeError := testutil.ToFloat64(metrics.DeploymentStatusTotal.WithLabelValues("error"))

	cfg := &agentcfg.Config{Hostname: "test-host", RepoURL: "https://example.com/repo.git"}
	reporter := agentmocks.NewMockDeploymentReporter(t)
	reporter.EXPECT().ReportError(mock.Anything, int64(7), mock.Anything).Return(nil)

	a := newTestAgent(t, cfg, WithDeploymentReporter(reporter))
	a.reportDeploymentResult(context.Background(), 7, fmt.Errorf("%w: apply failed", errRollbackPerformed))

	afterError := testutil.ToFloat64(metrics.DeploymentStatusTotal.WithLabelValues("error"))
	assert.InDelta(t, beforeError+1, afterError, 0.001)
}

func TestReportDeploymentResultUsesDetachedContextOnSuccess(t *testing.T) {
	t.Parallel()
	metrics.Register(nil)

	cfg := &agentcfg.Config{Hostname: "test-host", RepoURL: "https://example.com/repo.git"}
	reporter := agentmocks.NewMockDeploymentReporter(t)
	reporter.EXPECT().ReportSuccess(mock.Anything, int64(9)).RunAndReturn(func(ctx context.Context, _ int64) error {
		assert.NoError(t, ctx.Err(), "terminal deployment report should not inherit parent cancellation")
		return nil
	})

	a := newTestAgent(t, cfg, WithDeploymentReporter(reporter))

	parentCtx, cancel := context.WithCancel(context.Background())
	cancel()
	a.reportDeploymentResult(parentCtx, 9, nil)
}

func TestShouldReportDeploymentError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "rollback error", err: fmt.Errorf("%w: boom", errRollbackPerformed), want: true},
		{name: "context canceled", err: context.Canceled, want: true},
		{name: "context deadline exceeded", err: context.DeadlineExceeded, want: true},
		{name: "normal error", err: errors.New("validation failed"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, shouldReportDeploymentError(tt.err))
		})
	}
}
