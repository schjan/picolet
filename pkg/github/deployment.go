package github

import (
	"context"
	"fmt"

	gogithub "github.com/google/go-github/v84/github"
)

const maxDescriptionLen = 140

// DeploymentReporter reports deployment lifecycle to GitHub Environments.
type DeploymentReporter struct {
	client      *Client
	environment string
}

// NewDeploymentReporter creates a reporter for the given environment (hostname).
func NewDeploymentReporter(client *Client, environment string) *DeploymentReporter {
	return &DeploymentReporter{
		client:      client,
		environment: environment,
	}
}

// CreateDeployment creates a GitHub deployment for the given SHA and sets its status to pending.
func (r *DeploymentReporter) CreateDeployment(ctx context.Context, sha string) (int64, error) {
	deployment, _, err := r.client.gh.Repositories.CreateDeployment(ctx, r.client.Owner, r.client.Repo, &gogithub.DeploymentRequest{
		Ref:              &sha,
		Environment:      &r.environment,
		AutoMerge:        new(false),
		RequiredContexts: &[]string{},
		Description:      new("picolet deployment"),
	})
	if err != nil {
		return 0, fmt.Errorf("creating deployment: %w", err)
	}

	if err := r.createStatus(ctx, deployment.GetID(), "pending", "Deployment queued"); err != nil {
		return deployment.GetID(), fmt.Errorf("setting pending status: %w", err)
	}

	return deployment.GetID(), nil
}

// ReportInProgress updates the deployment status to in_progress.
func (r *DeploymentReporter) ReportInProgress(ctx context.Context, deploymentID int64) error {
	return r.createStatus(ctx, deploymentID, "in_progress", "Applying changes")
}

// ReportSuccess updates the deployment status to success.
func (r *DeploymentReporter) ReportSuccess(ctx context.Context, deploymentID int64) error {
	return r.createStatus(ctx, deploymentID, "success", "Deployment successful")
}

// ReportFailure updates the deployment status to failure with an error description.
func (r *DeploymentReporter) ReportFailure(ctx context.Context, deploymentID int64, deployErr error) error {
	return r.createStatus(ctx, deploymentID, "failure", truncate(deployErr.Error(), maxDescriptionLen))
}

// ReportError updates the deployment status to error (e.g. rollback occurred).
func (r *DeploymentReporter) ReportError(ctx context.Context, deploymentID int64, deployErr error) error {
	return r.createStatus(ctx, deploymentID, "error", truncate(deployErr.Error(), maxDescriptionLen))
}

func (r *DeploymentReporter) createStatus(ctx context.Context, deploymentID int64, state, description string) error {
	_, _, err := r.client.gh.Repositories.CreateDeploymentStatus(ctx, r.client.Owner, r.client.Repo, deploymentID, &gogithub.DeploymentStatusRequest{
		State:       &state,
		Description: &description,
	})
	if err != nil {
		return fmt.Errorf("creating %s status: %w", state, err)
	}

	return nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}

	return s[:maxLen-3] + "..."
}
