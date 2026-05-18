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
	"path"
	"strings"
	"sync"
)

// DefaultCLIPath is the path looked up via PATH when ClientConfig.CLIPath is empty.
const DefaultCLIPath = "pass-cli"

// DefaultSessionDir is the path used in PAT mode when ClientConfig.SessionDir is empty.
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
// (PROTON_PASS_SESSION_DIR). Empty means "use pass-cli's default" in lazy mode
// and DefaultSessionDir in PAT mode.
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
	runner     cmdRunner
	cliPath    string
	env        []string // allowlisted env with controlled PROTON_PASS_* overlays
	patPath    string   // empty = lazy mode (use existing user session)
	sessionDir string
	sessionMu  sync.Mutex
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
		// Duplicate effectiveSessionDir after buildBaseEnv so directory creation
		// stays tied to session use instead of construction.
		sessionDir: effectiveSessionDir(cfg),
	}, nil
}

// buildBaseEnv constructs the environment for pass-cli calls.
// It preserves only the host variables pass-cli commonly needs for PATH, HOME,
// keyrings, proxy, and CA behavior, then applies Picolet-controlled
// PROTON_PASS_* overlays.
func buildBaseEnv(cfg ClientConfig) ([]string, error) {
	env := allowedHostEnv()

	var encKey string
	if cfg.PATPath != "" {
		if cfg.EncryptionKeyPath == "" {
			return nil, errors.New("protonpass: encryption_key_path is required when pat_path is set")
		}
		keyBytes, err := os.ReadFile(cfg.EncryptionKeyPath)
		if err != nil {
			return nil, fmt.Errorf("reading encryption key: %w", err)
		}
		encKey = strings.TrimSpace(string(keyBytes))
		if encKey == "" {
			return nil, errors.New("protonpass: encryption key file is empty")
		}
	}

	if sessionDir := effectiveSessionDir(cfg); sessionDir != "" {
		env = append(env, "PROTON_PASS_SESSION_DIR="+sessionDir)
	}
	env = append(env, "PROTON_PASS_NO_UPDATE_CHECK=1")

	if encKey != "" {
		env = append(env,
			"PROTON_PASS_KEY_PROVIDER=env",
			"PROTON_PASS_ENCRYPTION_KEY="+encKey,
		)
	}
	return env, nil
}

func effectiveSessionDir(cfg ClientConfig) string {
	if cfg.SessionDir != "" {
		return cfg.SessionDir
	}
	if cfg.PATPath != "" {
		return DefaultSessionDir
	}
	return ""
}

func ensureSessionDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func allowedHostEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !isAllowedEnv(key) {
			continue
		}
		env = append(env, entry)
	}
	if os.Getenv("PATH") == "" {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return env
}

func isAllowedEnv(key string) bool {
	switch key {
	case "PATH",
		"HOME",
		"USER",
		"LOGNAME",
		"LANG",
		"LC_ALL",
		"TMPDIR",
		"TEMP",
		"TMP",
		"XDG_RUNTIME_DIR",
		"XDG_CONFIG_HOME",
		"XDG_CACHE_HOME",
		"XDG_DATA_HOME",
		"DBUS_SESSION_BUS_ADDRESS",
		"SSL_CERT_FILE",
		"SSL_CERT_DIR",
		"NIX_SSL_CERT_FILE",
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"ALL_PROXY",
		"NO_PROXY",
		"http_proxy",
		"https_proxy",
		"all_proxy",
		"no_proxy":
		return true
	default:
		return false
	}
}

// EnsureSession probes for an active session and, if missing AND a PAT is
// configured, performs a non-interactive login and verifies the resulting
// session. It intentionally checks every call so expired sessions are detected
// before secret resolution.
//
// When no session is active and no PAT is configured (Lazy mode), returns a
// clear error pointing the operator at the missing configuration.
func (c *Client) EnsureSession(ctx context.Context) error {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()

	if c.sessionDir != "" {
		if err := ensureSessionDir(c.sessionDir); err != nil {
			return fmt.Errorf("creating protonpass session dir: %w", err)
		}
	}

	probeErr := c.probeSession(ctx)
	if probeErr == nil {
		slog.Debug("protonpass session check passed")
		return nil
	}

	if c.patPath == "" {
		return fmt.Errorf("protonpass: no active session and no PAT configured for login: %w", probeErr)
	}

	slog.Info("protonpass session not active, logging in", "cli_path", c.cliPath)
	if err := c.loginLocked(ctx); err != nil {
		return err
	}
	if err := c.probeSession(ctx); err != nil {
		return fmt.Errorf("protonpass login completed but session check failed: %w", err)
	}
	slog.Info("protonpass login completed")
	return nil
}

// probeSession runs `pass-cli test` and reports whether the call
// succeeded. A non-nil return value means there is no usable session.
func (c *Client) probeSession(ctx context.Context) error {
	_, stderr, err := c.runner.Run(ctx, c.env, c.cliPath, "test")
	if err != nil {
		if msg := sanitizeStderr(stderr); msg != "" {
			return fmt.Errorf("%s: %w", msg, err)
		}
		return err
	}
	return nil
}

// loginLocked reads the PAT from disk and runs `pass-cli login` with the
// token supplied via PROTON_PASS_PERSONAL_ACCESS_TOKEN (env, never argv).
// Caller must hold sessionMu.
func (c *Client) loginLocked(ctx context.Context) error {
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
//
// Invalid refs are rejected inside resolveLocked via ParseRef, so a bogus
// input takes the EnsureSession round-trip before failing — an acceptable
// trade for keeping a single validation point that also covers ResolveAll.
func (c *Client) Resolve(ctx context.Context, ref string) (string, error) {
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
	parsed, err := ParseRef(ref)
	if err != nil {
		return "", fmt.Errorf("protonpass resolve %q: %w", ref, err)
	}
	stdout, stderr, err := c.runner.Run(ctx, c.env, c.cliPath, "item", "view", ref, "--output=json")
	if err != nil {
		return "", fmt.Errorf("protonpass resolve %q: %s: %w", ref, sanitizeStderr(stderr), err)
	}
	val, err := parseItemViewJSON(stdout, parsed)
	if err != nil {
		return "", fmt.Errorf("protonpass resolve %q: %w", ref, err)
	}
	return val, nil
}

// parseItemViewJSON extracts a string secret from `pass-cli item view --output=json`.
//
// Accepted shapes:
//   - a JSON-encoded string ("password-value")
//   - an object with a single string-typed field
//   - an object with multiple fields, of which one's key matches the tail
//     segment of ref.Field (path.Base) — picked by name to survive future
//     pass-cli versions that add metadata alongside the value.
func parseItemViewJSON(stdout []byte, ref PassRef) (string, error) {
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
	if len(asObj) == 1 {
		for _, v := range asObj {
			s, ok := v.(string)
			if !ok {
				return "", fmt.Errorf("pass-cli output: field value is %T, want string", v)
			}
			return s, nil
		}
	}
	wantKey := path.Base(ref.Field)
	val, ok := asObj[wantKey]
	if !ok {
		return "", fmt.Errorf("pass-cli output: field %q not present in response", wantKey)
	}
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("pass-cli output: field %q value is %T, want string", wantKey, val)
	}
	return s, nil
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
