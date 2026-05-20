package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/schjan/picolet/pkg/metrics"
)

const deploymentReportTimeout = 10 * time.Second

// createDeployment creates a GitHub deployment and reports in_progress if a reporter is configured.
// Returns 0 when no reporter is set or when deployment creation itself fails.
func (a *Agent) createDeployment(ctx context.Context, sha string) int64 {
	if a.deployReporter == nil {
		return 0
	}
	deploymentID, err := a.deployReporter.CreateDeployment(ctx, sha)
	if err == nil {
		metrics.DeploymentStatusTotal.WithLabelValues("pending").Inc()
	}
	if err != nil {
		slog.Warn("deployment status: create failed", "error", err)
		metrics.DeploymentStatusTotal.WithLabelValues("api_error").Inc()
		if deploymentID == 0 {
			return 0
		}
		slog.Info("deployment status: continuing with created deployment despite pending status error", "deployment_id", deploymentID)
	}

	if err := a.deployReporter.ReportInProgress(ctx, deploymentID); err != nil {
		slog.Warn("deployment status: in_progress failed", "error", err)
		metrics.DeploymentStatusTotal.WithLabelValues("api_error").Inc()
	} else {
		metrics.DeploymentStatusTotal.WithLabelValues("in_progress").Inc()
	}
	return deploymentID
}

// reportDeploymentResult reports the final deployment status (success/failure) if a deployment was created.
func (a *Agent) reportDeploymentResult(ctx context.Context, deploymentID int64, reconcileErr error) {
	if deploymentID == 0 || a.deployReporter == nil {
		return
	}
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deploymentReportTimeout)
	defer cancel()

	if reconcileErr == nil {
		a.reportDeploymentSuccess(reportCtx, deploymentID)
		return
	}
	label := "failure"
	report := a.deployReporter.ReportFailure
	if shouldReportDeploymentError(reconcileErr) {
		label = "error"
		report = a.deployReporter.ReportError
	}
	a.reportTerminalStatus(reportCtx, deploymentID, reconcileErr, label, report)
}

func (a *Agent) reportDeploymentSuccess(ctx context.Context, deploymentID int64) {
	if err := a.deployReporter.ReportSuccess(ctx, deploymentID); err != nil {
		slog.Warn("deployment status: success report failed", "error", err)
		metrics.DeploymentStatusTotal.WithLabelValues("api_error").Inc()
		return
	}
	metrics.DeploymentStatusTotal.WithLabelValues("success").Inc()
}

// reportTerminalStatus reports the terminal status of a reconcile (failure or
// error) and increments the matching DeploymentStatusTotal label.
// shouldReportDeploymentError picks the label upstream.
func (a *Agent) reportTerminalStatus(ctx context.Context, deploymentID int64, reconcileErr error, label string, report func(context.Context, int64, error) error) {
	if err := report(ctx, deploymentID, reconcileErr); err != nil {
		slog.Warn("deployment status: "+label+" report failed", "error", err)
		metrics.DeploymentStatusTotal.WithLabelValues("api_error").Inc()
		return
	}
	metrics.DeploymentStatusTotal.WithLabelValues(label).Inc()
}

// shouldReportDeploymentError distinguishes the two GitHub Deployment error
// states. ReportError is for transient/infrastructural failures (cancellation,
// timeouts, rollback performed); ReportFailure is for genuine deploy failures.
func shouldReportDeploymentError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return errors.Is(err, errRollbackPerformed)
}
