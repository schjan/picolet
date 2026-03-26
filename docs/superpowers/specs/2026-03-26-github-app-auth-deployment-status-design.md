# GitHub App Authentication & Deployment Status Reporting

## Problem

Picolet currently authenticates to GitHub using Personal Access Tokens (PATs) for git operations. There is no mechanism to report deployment status back to GitHub. Fleet operators cannot see in the GitHub UI whether a given commit was successfully applied to each Pi.

## Goals

1. Replace PAT-based git authentication with GitHub App authentication (short-lived tokens, not tied to a personal account, scoped permissions).
2. Report deployment lifecycle status to GitHub Environments so operators see per-instance deployment results directly in the GitHub UI.
3. Design for future 1Password integration (dynamic secret loading) without requiring it now.

## Non-Goals

- GitHub Environment protection rules (required reviewers, wait timers).
- Webhook-triggered deployments (picolet remains poll-based).
- Supporting non-GitHub hosting providers (GitLab, Bitea, etc.).
- Replacing SSH agent auth (remains supported, but cannot do API calls).

## Background

### GitHub App Authentication Model

A GitHub App provides machine-identity authentication:

1. Create one GitHub App (per org/account) with permissions: `contents:read` + `deployments:read_write`.
2. Install it on the fleet repository. This produces an **Installation ID**.
3. Each picolet instance holds the same **App ID**, **Installation ID**, and **private key** (PEM file).
4. To authenticate, each instance independently:
   - Signs a JWT locally with the private key (RS256, valid 10 minutes).
   - Exchanges the JWT for an installation token via GitHub API (valid 1 hour).
   - Uses the installation token for git operations and API calls.
5. No central component needed. Fully decentralized. Works after arbitrary offline periods (just generate fresh tokens on reconnect).

### GitHub Deployments API

The Deployments API has three levels:

- **Environment** — a named deployment target (e.g., `iuk-srv-1`, `iuk-srv-1-system`). Auto-created on first deployment.
- **Deployment** — a specific SHA being deployed to an environment.
- **Deployment Status** — a state update on a deployment: `pending`, `in_progress`, `success`, `failure`, `error`.

GitHub shows the latest deployment status per environment on the repository page.

### Current State

- Git auth: PAT read from file (`git_token_path` in agent config) or SSH agent.
- No GitHub API integration exists (only webhook HMAC validation).
- MQTT already publishes reconciliation status (applied SHA, failure count).
- Fleet instances are uniquely identified by hostname (e.g., `iuk-srv-1` for rootless, `iuk-srv-1-system` for rootful).

## Design

### GitHub App Auth (`pkg/github/client.go`)

Use the established library ecosystem rather than implementing JWT signing:

- **`bradleyfalzon/ghinstallation/v2`** — handles JWT signing, installation token exchange, caching, and auto-refresh. Implements `http.RoundTripper`.
- **`google/go-github/v84`** — GitHub REST API client, accepts a custom `http.Client` with the ghinstallation transport.

```go
// Client wraps go-github with GitHub App auth.
type Client struct {
    gh          *github.Client
    transport   *ghinstallation.Transport // exposes Token(ctx) for git auth
    owner, repo string                    // parsed from repo URL
}

func NewClient(appID, installationID int64, privateKeyPath string, repoURL string) (*Client, error)
```

`Client` also implements the `AuthProvider` interface (see below) so it can be used for git operations.

#### 1Password Future Path

`ghinstallation` supports `WithSigner(signer)` for custom JWT signing. When 1Password integration lands, a custom `Signer` implementation can fetch the private key dynamically from 1Password instead of reading from disk. No changes to `Client` or the rest of the codebase.

### Git Auth Abstraction (`pkg/gitpoll`)

Extract an `AuthProvider` interface from the current inline auth logic in `Poller`. The interface lives in `pkg/gitpoll` (consumer-owned), and returns `transport.AuthMethod` from `go-git/v5/plumbing/transport`:

```go
// AuthProvider provides authentication for git operations.
// Defined in pkg/gitpoll. Implementations: fileTokenAuth (PAT), sshAgentAuth,
// and github.Client (which imports go-git's transport package to satisfy this).
type AuthProvider interface {
    GitAuth(ctx context.Context) (transport.AuthMethod, error)
}
```

The current PAT/SSH logic becomes concrete implementations within `pkg/gitpoll` (unexported). `github.Client` becomes another implementation — it imports `go-git/v5/plumbing/transport` and calls `transport.Token(ctx)` to return `*http.BasicAuth{Username: "x-access-token", Password: token}`. This means `pkg/github` depends on `pkg/gitpoll` (for the interface) and on `go-git` (for `transport.AuthMethod`). `pkg/gitpoll` has no dependency on `pkg/github`.

**Constructor change:** `Poller.New` signature changes from `New(repoURL, branch, localPath, tokenPath string)` to `New(repoURL, branch, localPath string, auth AuthProvider)`. Call sites in `pkg/agent` and `cmd/picolet` are updated accordingly. The `tokenPath` field is removed from the `Poller` struct.

### Deployment Status Reporting (`pkg/github/deployment.go`)

```go
// DeploymentReporter reports deployment lifecycle to GitHub Environments.
type DeploymentReporter struct {
    client      *Client
    environment string // hostname from agent config
}
```

#### Lifecycle Methods

| Reconciliation Stage     | Method                                            | GitHub Status  |
| ------------------------ | ------------------------------------------------- | -------------- |
| New SHA detected         | `CreateDeployment(ctx, sha) (deploymentID, error)`| `pending`      |
| Apply starts             | `ReportInProgress(ctx, deploymentID) error`       | `in_progress`  |
| Apply succeeds           | `ReportSuccess(ctx, deploymentID) error`          | `success`      |
| Apply fails              | `ReportFailure(ctx, deploymentID, err) error`     | `failure`      |
| Rollback triggered       | `ReportError(ctx, deploymentID, err) error`       | `error`        |

`CreateDeployment` makes two API calls:
1. `Repositories.CreateDeployment` — `Ref: sha`, `Environment: hostname`, `AutoMerge: false`, `RequiredContexts: &[]string{}` (picolet does its own validation).
2. `Repositories.CreateDeploymentStatus` — `State: "pending"`.

`ReportFailure` and `ReportError` include a truncated error message in the `Description` field (GitHub caps at 140 characters).

Optional: if `external_hostname` is set in host config, `LogURL` is set to `http://<external_hostname>:<metrics_port>/` on all statuses.

#### Failure Modes

Deployment status reporting is **best-effort**:
- If a GitHub API call fails, log the error.
- Never fail the reconciliation because of a reporting error.
- Do not retry within the same tick (next tick creates a fresh deployment if SHA changed).

### Consumer Interface (`pkg/agent`)

Following the codebase convention where consumer packages own interfaces:

```go
// DeploymentReporter reports deployment lifecycle to an external system.
type DeploymentReporter interface {
    CreateDeployment(ctx context.Context, sha string) (int64, error)
    ReportInProgress(ctx context.Context, deploymentID int64) error
    ReportSuccess(ctx context.Context, deploymentID int64) error
    ReportFailure(ctx context.Context, deploymentID int64, err error) error
    ReportError(ctx context.Context, deploymentID int64, err error) error
}
```

`github.DeploymentReporter` satisfies this interface. The agent holds an optional (nilable) `DeploymentReporter`.

### Reconciliation Pipeline Changes

**Key decision: deployment reporting stays in `tick()`, not inside `ReconcileOnce()`.** `ReconcileOnce()` remains unchanged — it bundles load/resolve/diff/validate/apply/save and is also used by `runApply` in `cmd/picolet/main.go`. Deployment reporting wraps the `ReconcileOnce()` call in `tick()`:

```go
// Simplified tick() flow:
deploymentID, _ := reporter.CreateDeployment(ctx, sha)  // pending
reporter.ReportInProgress(ctx, deploymentID)              // in_progress
result, err := a.ReconcileOnce(ctx, sha, st, store)
if err != nil {
    reporter.ReportFailure(ctx, deploymentID, err)        // failure
} else {
    reporter.ReportSuccess(ctx, deploymentID)             // success
}
```

This means `in_progress` fires just before `ReconcileOnce` (including validation time), not between validation and apply. The practical difference is negligible — both happen within the same tick.

Modified `tick()` flow (new steps in bold):

1. Health enforce
2. Git poll (now via `AuthProvider` — GitHub App or PAT/SSH)
3. Failure gate
4. **Create deployment with `pending` status** (if SHA changed and reporter != nil)
5. **Report `in_progress`**
6. `ReconcileOnce()` (load → resolve → diff → validate → apply → save state)
7. **Report `success` or `failure`/`error` based on `ReconcileOnce` result**
8. MQTT status

The deployment ID is a local variable in `tick()`. Not persisted to the state file.

**Shutdown handling:** A `defer` at the top of `tick()` checks `ctx.Err() != nil && deploymentID != 0` and sends a best-effort `error` status using a detached context with a 10-second timeout (following the existing `applyWithRollback` pattern that uses `context.WithoutCancel`).

#### Edge Cases

| Scenario                        | Behavior                                                                           |
| ------------------------------- | ---------------------------------------------------------------------------------- |
| SHA unchanged (no-op tick)      | No deployment created, reporting skipped                                           |
| Failure gate blocks             | No deployment created (already reported failure on previous ticks)                 |
| Validation fails                | `ReconcileOnce` returns error → report `failure` with validation error summary     |
| Apply fails + rollback          | `ReconcileOnce` returns error → report `error` with rollback context               |
| Context cancelled (shutdown)    | Defer sends best-effort `error` via detached context (10s timeout)                 |
| GitHub API unreachable          | Log warning, continue reconciliation normally                                      |
| Restart mid-tick                | Stale `pending`/`in_progress` in GitHub; next successful tick creates fresh deployment |

### Configuration (`pkg/agentcfg`)

Three new fields:

```go
GitHubAppID          int64  `yaml:"github_app_id"`
GitHubInstallationID int64  `yaml:"github_installation_id"`
GitHubPrivateKeyPath string `yaml:"github_private_key_path"`
```

Validation in `agentcfg.Validate()`:
- All three must be set together (partial config is an error).
- Mutually exclusive with `git_token_path` (error if both set).
- `github_app_id` and `github_installation_id` must be positive.
- Presence of GitHub App config automatically enables deployment status reporting.

Validation in `cmd/picolet` wiring code (not in `agentcfg`):
- If GitHub App is configured, `repo_url` must be an HTTPS GitHub URL. SSH GitHub URLs are rejected because `github.Client.GitAuth()` returns `*http.BasicAuth` which only works over HTTPS.
- `github_private_key_path` must point to a valid, readable PEM file (fail-fast at startup rather than on first auth attempt).
- Owner/repo parsing from `repo_url` is validated here.

Example config:

```yaml
hostname: "iuk-srv-1"
repo_url: "https://github.com/drk-darmstadt-iuk/iuk-gitops.git"
repo_branch: "main"
poll_interval: "60s"

# GitHub App auth (replaces git_token_path)
github_app_id: 12345
github_installation_id: 67890
github_private_key_path: "/etc/picolet/secrets/github-app.pem"
```

### Repo URL Parsing (`pkg/github/url.go`)

Parses `owner` and `repo` from the configured `repo_url`:

- `https://github.com/org/repo.git` -> `org`, `repo`

Returns an error for non-GitHub URLs or non-HTTPS GitHub URLs when GitHub App auth is configured. SSH-style URLs (`git@github.com:...`) are rejected because GitHub App tokens require HTTPS.

## Package Structure

```
pkg/github/
  client.go        # Client, NewClient, AuthProvider implementation
  deployment.go    # DeploymentReporter, lifecycle methods
  url.go           # ParseRepoURL
```

### Changes to Existing Packages

| Package        | Change                                                                          |
| -------------- | ------------------------------------------------------------------------------- |
| `pkg/gitpoll`  | Extract `AuthProvider` interface, refactor `Poller` to accept it                |
| `pkg/agentcfg` | Add 3 config fields + validation (mutual exclusion with `git_token_path`)       |
| `pkg/agent`    | Add `DeploymentReporter` interface, thread deployment ID through `tick()`       |
| `cmd/picolet`  | Wire GitHub App config: create `Client` -> `AuthProvider` + `DeploymentReporter` |

### Unchanged Packages

`pkg/state`, `pkg/config`, `pkg/resolver`, `pkg/applier`, `pkg/health`, `pkg/rollback`, `pkg/validator`, `pkg/metrics`, `pkg/mqtt`, `pkg/orphan`.

## New Dependencies

- `github.com/google/go-github/v84` (or latest at implementation time)
- `github.com/bradleyfalzon/ghinstallation/v2`

## Metrics

Add a Prometheus counter to `pkg/metrics`:

- `picolet_deployment_status_total` — counter with labels `status` (`pending`, `in_progress`, `success`, `failure`, `error`) and `result` (`ok`, `api_error`). Follows the existing pattern of `GitPollTotal`, `ReconciliationTotal`, etc.

## Testing

- **`pkg/github`**: Unit tests with `httptest.NewServer` mocking GitHub API responses. Test token refresh, deployment lifecycle, error handling, URL parsing.
- **`pkg/gitpoll`**: Existing tests updated to use `AuthProvider` interface (mock implementation).
- **`pkg/agent`**: Agent integration test extended with a mock `DeploymentReporter` to verify lifecycle calls happen in correct order. Add `DeploymentReporter` to `.mockery.yml` under `pkg/agent` (alongside existing `MQTTClient` mock).

## Dependency Considerations

Both `google/go-github` and `bradleyfalzon/ghinstallation` are pure Go with no CGO requirements. The binary size impact should be verified during implementation but is expected to be modest given the project already depends on `go-git` and Podman bindings. Cross-compilation to `linux/arm64` is unaffected since `CGO_ENABLED=0` is already required.
