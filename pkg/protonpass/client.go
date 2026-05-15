package protonpass

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

// DefaultCLIPath is the path looked up via PATH when ClientConfig.CLIPath is empty.
const DefaultCLIPath = "pass-cli"

// DefaultSessionDir is the path used when ClientConfig.SessionDir is empty.
const DefaultSessionDir = "/var/lib/picolet/protonpass/.session"

// cmdRunner abstracts exec.CommandContext so unit tests can supply a fake
// process runner. Callers receive stdout, stderr, and the exec error.
type cmdRunner interface {
	Run(ctx context.Context, env []string, name string, args ...string) (stdout, stderr []byte, err error)
}

// ClientConfig configures the Proton Pass client.
//
// CLIPath is the path to the pass-cli binary (default "pass-cli", looked up via PATH).
//
// PATPath, when non-empty, enables auto-login: at first session probe failure
// the client logs in non-interactively using the PAT read from this file. When
// empty (Lazy mode), the client uses any existing user session and never
// attempts to log in.
//
// EncryptionKeyPath is required when PATPath is set. The file contents are
// passed via PROTON_PASS_ENCRYPTION_KEY so pass-cli can store/read its session
// state with the env-mode key provider.
//
// SessionDir isolates the pass-cli session from the host user's session
// (PROTON_PASS_SESSION_DIR). Empty means "use the system default" — typically
// only desirable for local development.
type ClientConfig struct {
	CLIPath           string
	PATPath           string
	EncryptionKeyPath string
	SessionDir        string
}

// Client wraps the pass-cli binary for batch secret resolution.
//
// Concurrency: the client serializes Resolve and ResolveAll via sessionMu
// because pass-cli is not documented as concurrent-safe and shares
// mutable session state on disk.
type Client struct {
	runner       cmdRunner
	cliPath      string
	env          []string // base env: PATH + PROTON_PASS_* vars
	patPath      string   // empty = lazy mode (use existing user session)
	sessionReady atomic.Bool
	sessionMu    sync.Mutex
}

// NewClient constructs a Client from ClientConfig.
//
// The encryption key file is read once at construction; the value is held
// in memory for the client's lifetime to be supplied to pass-cli on each
// invocation via PROTON_PASS_ENCRYPTION_KEY.
func NewClient(cfg ClientConfig) (*Client, error) {
	cliPath := cfg.CLIPath
	if cliPath == "" {
		cliPath = DefaultCLIPath
	}
	env, err := buildBaseEnv(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{
		runner:  execRunner{},
		cliPath: cliPath,
		env:     env,
		patPath: cfg.PATPath,
	}, nil
}

// buildBaseEnv constructs the explicit, minimal env slice for pass-cli calls.
// Excludes the host's environment to avoid leaking secrets or PATs unintentionally.
func buildBaseEnv(cfg ClientConfig) ([]string, error) {
	env := []string{"PATH=" + os.Getenv("PATH")}

	if cfg.SessionDir != "" {
		env = append(env, "PROTON_PASS_SESSION_DIR="+cfg.SessionDir)
	}

	if cfg.PATPath == "" {
		return env, nil
	}

	if cfg.EncryptionKeyPath == "" {
		return nil, errors.New("ProtonPass: encryption_key_path is required when pat_path is set")
	}
	keyBytes, err := os.ReadFile(cfg.EncryptionKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading encryption key: %w", err)
	}
	encKey := strings.TrimSpace(string(keyBytes))
	if encKey == "" {
		return nil, errors.New("ProtonPass: encryption key file is empty")
	}
	env = append(env,
		"PROTON_PASS_KEY_PROVIDER=env",
		"PROTON_PASS_ENCRYPTION_KEY="+encKey,
	)
	return env, nil
}

// EnsureSession is idempotent: it probes for an active session and, if missing
// AND a PAT is configured, performs a non-interactive login. Subsequent calls
// short-circuit via sessionReady once a successful probe or login completes.
//
// When no session is active and no PAT is configured (Lazy mode), returns a
// clear error pointing the operator at the missing configuration.
func (c *Client) EnsureSession(ctx context.Context) error {
	if c.sessionReady.Load() {
		return nil
	}
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	if c.sessionReady.Load() {
		return nil
	}

	if err := c.probeSession(ctx); err == nil {
		slog.Debug("protonpass session check passed")
		c.sessionReady.Store(true)
		return nil
	}

	if c.patPath == "" {
		return errors.New("protonpass: no active session and no PAT configured for login")
	}

	slog.Info("protonpass session not active, logging in", "cli_path", c.cliPath)
	if err := c.ensureLoginLocked(ctx); err != nil {
		return err
	}
	slog.Info("protonpass login completed")
	c.sessionReady.Store(true)
	return nil
}

// probeSession runs `pass-cli vault list` and reports whether the call
// succeeded. A non-nil return value means there is no usable session.
func (c *Client) probeSession(ctx context.Context) error {
	_, _, err := c.runner.Run(ctx, c.env, c.cliPath, "vault", "list")
	return err
}

// ensureLoginLocked reads the PAT from disk and runs `pass-cli login` with the
// token supplied via PROTON_PASS_PERSONAL_ACCESS_TOKEN (env, never argv).
// Caller must hold sessionMu.
func (c *Client) ensureLoginLocked(ctx context.Context) error {
	patBytes, err := os.ReadFile(c.patPath)
	if err != nil {
		return fmt.Errorf("reading proton pass PAT: %w", err)
	}
	pat := strings.TrimSpace(string(patBytes))
	if pat == "" {
		return errors.New("protonpass: PAT file is empty")
	}
	loginEnv := append(append([]string{}, c.env...), "PROTON_PASS_PERSONAL_ACCESS_TOKEN="+pat)
	_, stderr, err := c.runner.Run(ctx, loginEnv, c.cliPath, "login")
	if err != nil {
		return fmt.Errorf("protonpass login failed: %s: %w", sanitizeStderr(stderr), err)
	}
	return nil
}

// Resolve returns the secret value for a single pass:// reference.
// Calls EnsureSession on first use. Output is parsed from --output=json
// rather than human-readable form so we can rely on a stable schema.
func (c *Client) Resolve(ctx context.Context, ref string) (string, error) {
	if !IsRef(ref) {
		return "", fmt.Errorf("protonpass: %q is not a valid pass:// reference", ref)
	}
	if err := c.EnsureSession(ctx); err != nil {
		return "", err
	}
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	return c.resolveLocked(ctx, ref)
}

// ResolveAll resolves multiple references in sequence. Per-reference errors
// are aggregated via errors.Join so callers can use partial results.
//
// pass-cli has no batch API; we serialize calls to avoid concurrent session
// state issues.
func (c *Client) ResolveAll(ctx context.Context, refs []string) (map[string]string, error) {
	if len(refs) == 0 {
		return nil, nil //nolint:nilnil // empty input → no work
	}
	if err := c.EnsureSession(ctx); err != nil {
		return nil, err
	}
	slog.Debug("protonpass batch resolving", "ref_count", len(refs))
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()

	results := make(map[string]string, len(refs))
	var errs []error
	for _, ref := range refs {
		val, err := c.resolveLocked(ctx, ref)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		results[ref] = val
	}
	return results, errors.Join(errs...)
}

// resolveLocked invokes pass-cli for one ref. Caller must hold sessionMu.
func (c *Client) resolveLocked(ctx context.Context, ref string) (string, error) {
	stdout, stderr, err := c.runner.Run(ctx, c.env, c.cliPath, "item", "view", ref, "--output=json")
	if err != nil {
		return "", fmt.Errorf("protonpass resolve %q: %s: %w", ref, sanitizeStderr(stderr), err)
	}
	val, err := parseItemViewJSON(stdout)
	if err != nil {
		return "", fmt.Errorf("protonpass resolve %q: %w", ref, err)
	}
	return val, nil
}

// parseItemViewJSON extracts a string secret from `pass-cli item view --output=json`.
//
// The CLI's output for a single field returns either:
//   - a JSON-encoded string ("password-value")
//   - an object with a single field (e.g. {"password": "..."})
//
// This parser handles both shapes and rejects non-string / multi-field results
// to surface unexpected schema changes early.
func parseItemViewJSON(stdout []byte) (string, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return "", errors.New("empty pass-cli output")
	}

	var asString string
	if err := json.Unmarshal(trimmed, &asString); err == nil {
		return asString, nil
	}

	var asObj map[string]any
	if err := json.Unmarshal(trimmed, &asObj); err != nil {
		return "", fmt.Errorf("decoding pass-cli output: %w", err)
	}
	if len(asObj) != 1 {
		return "", fmt.Errorf("pass-cli output: expected single field, got %d", len(asObj))
	}
	for _, v := range asObj {
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("pass-cli output: field value is %T, want string", v)
		}
		return s, nil
	}
	// Unreachable — len check above guarantees one entry.
	return "", errors.New("pass-cli output: unexpected empty object")
}

// execRunner is the production cmdRunner backed by os/exec.
type execRunner struct{}

// Run executes name with args under the given env, returning captured stdout/stderr.
// G204 is intentionally not suppressed: the binary path is operator-supplied,
// and ref arguments are validated by ParseRef before reaching this method.
func (execRunner) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}
