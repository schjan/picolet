package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
)

// initAgentBundleRepo creates a bare repo whose rootful test-host runs its
// own Agent bundle (picolet-system) with the given rendered Agent config.
func initAgentBundleRepo(t *testing.T, agentConfig string) (bareDir, headSHA string) {
	t.Helper()
	return initAgentBundleRepoWithUnit(t, agentBundleUnit(""), agentConfig)
}

// agentBundleUnit is the test Agent quadlet with the extra [Container] lines
// mounts (Volume=/Secret= lines).
func agentBundleUnit(mounts string) string {
	return `[Container]
Image=ghcr.io/schjan/picolet:latest
Secret=picolet_system_config,target=/etc/picolet/config.yml
` + mounts
}

// initAgentBundleRepoWithUnit is initAgentBundleRepo with the Agent quadlet's
// content.
func initAgentBundleRepoWithUnit(t *testing.T, unit, agentConfig string) (bareDir, headSHA string) {
	t.Helper()
	bareDir = filepath.Join(t.TempDir(), "bare.git")
	workDir := filepath.Join(t.TempDir(), "work")
	repo, err := git.PlainInit(workDir, false)
	require.NoError(t, err)

	writeTestFile(t, workDir, "fleet.yml", "images: {}\nports: {picolet_system_metrics: 9418}\n")
	writeTestFile(t, workDir, "assignments.yml", "base:\n  services:\n    - picolet-system\n")
	writeTestFile(t, workDir, "hosts/test-host/host.yml", "hostname: test-host\nrole: server\nfeatures: []\n")
	writeTestFile(t, workDir, "services/picolet-system/picolet-system.container", unit)
	writeTestFile(t, workDir, "services/picolet-system/secrets/picolet_system_config.yml", agentConfig)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add(".")
	require.NoError(t, err)
	head, err := wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com"},
	})
	require.NoError(t, err)
	_, err = git.PlainClone(bareDir, true, &git.CloneOptions{URL: workDir})
	require.NoError(t, err)
	return bareDir, head.String()
}

// TestAgentRefusesSelfConfigItCouldNotStartWith runs the Agent loop on a
// Fleet that renders the Agent's own config: a config naming a missing
// credential file, or one this Agent cannot parse, fails the reconciliation
// before anything is written; a config whose credentials exist applies.
func TestAgentRefusesSelfConfigItCouldNotStartWith(t *testing.T) {
	t.Parallel()
	tokenDir := t.TempDir()
	present := filepath.Join(tokenDir, "git.token")
	require.NoError(t, os.WriteFile(present, []byte("token\n"), 0o600))
	missing := filepath.Join(tokenDir, "age-identity")

	tests := []struct {
		name      string
		config    string
		wantError []string // empty: the config applies
	}{
		{
			name:      "missing credential file",
			config:    "hostname: test-host\nrepo_url: https://example.com/fleet.git\nwebhook_secret_path: " + missing + "\n",
			wantError: []string{"webhook_secret_path", missing},
		},
		{
			name:      "missing age identity file",
			config:    "hostname: test-host\nrepo_url: https://example.com/fleet.git\nsops:\n  age_identity_path: " + missing + "\n",
			wantError: []string{"sops.age_identity_path", missing},
		},
		{
			name:      "unknown key",
			config:    "hostname: test-host\nrepo_url: https://example.com/fleet.git\nno_such_key_ever: x\n",
			wantError: []string{"no_such_key_ever"},
		},
		{
			name:   "credential files present",
			config: "hostname: test-host\nrepo_url: https://example.com/fleet.git\ngit_token_path: " + present + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runSelfConfigCase(t, tt.config, tt.wantError)
		})
	}
}

// runSelfConfigCase runs the Agent loop on a Fleet rendering agentConfig as
// the Agent's own config and checks the outcome: applied when wantError is
// empty, otherwise refused before anything is written, with an error naming
// every wantError fragment.
func runSelfConfigCase(t *testing.T, agentConfig string, wantError []string) {
	t.Helper()
	bareDir, headSHA := initAgentBundleRepo(t, agentConfig)
	statePath := filepath.Join(t.TempDir(), "state.json")

	sys, pod, fw := newBareMocks(t)
	setupNoopMocks(sys, pod)
	var written map[string][]byte
	var createdSecrets []string
	if len(wantError) == 0 {
		// Only the applying case may write; the strict mocks fail the
		// refusing cases on any write.
		written = setupApplyMocks(sys, pod, fw)
		pod.EXPECT().SecretExists(mock.Anything, mock.Anything).Return(false, nil).Maybe()
		pod.EXPECT().SecretCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, name string, _ []byte, _ bool) error {
				createdSecrets = append(createdSecrets, name)
				return nil
			}).Maybe()
		sys.EXPECT().StartUnit(mock.Anything, mock.Anything).Return(nil).Maybe()
	}

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: 200 * time.Millisecond,
		ListenAddr:   "127.0.0.1:0",
		SecretsDir:   t.TempDir(),
	}
	a := newTestAgent(t, cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(filepath.Join(t.TempDir(), "clone")),
		WithStatePath(statePath),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.NoError(t, a.Run(ctx))

	st, err := state.NewStore(statePath).Load()
	require.NoError(t, err)
	if len(wantError) == 0 {
		assert.Equal(t, headSHA, st.AppliedSHA)
		assert.Empty(t, st.FailedSHA)
		assert.Contains(t, createdSecrets, "picolet_system_config")
		assert.Contains(t, written, "/etc/containers/systemd/picolet/picolet-system.container")
		return
	}
	assert.Equal(t, headSHA, st.FailedSHA, "the reconciliation fails like any pre-apply failure")
	assert.Empty(t, st.AppliedSHA)
	assert.Empty(t, st.ManagedFiles, "nothing is written")

	_, err = a.ReconcileOnce(t.Context(), headSHA, state.NewState(), state.NewStore(filepath.Join(t.TempDir(), "state.json")))
	require.Error(t, err)
	for _, want := range wantError {
		assert.Contains(t, err.Error(), want)
	}
}

// TestAgentRefusesMountSwitchToSelfConfigItCouldNotStartWith covers a commit
// that only repoints the Agent quadlet's config mount at another secret the
// Fleet already manages: the secret itself is unchanged, but the Agent's next
// start reads it, so a config the Agent could not start with is refused.
func TestAgentRefusesMountSwitchToSelfConfigItCouldNotStartWith(t *testing.T) {
	t.Parallel()
	const (
		quadlet    = "services/picolet-system/picolet-system.container"
		nextConfig = "services/picolet-system/secrets/picolet_system_config_next.yml"
	)
	bareDir, _ := initAgentBundleRepo(t, "hostname: test-host\nrepo_url: https://example.com/fleet.git\n")
	pushToTestRepo(t, bareDir, map[string]string{
		nextConfig: "hostname: test-host\nrepo_url: https://example.com/fleet.git\nno_such_key_ever: x\n",
	})
	repoDir := filepath.Join(t.TempDir(), "clone")
	statePath := filepath.Join(t.TempDir(), "state.json")

	sys, pod, fw := newBareMocks(t)
	written := setupApplyMocks(sys, pod, fw)
	pod.EXPECT().SecretExists(mock.Anything, mock.Anything).Return(false, nil).Maybe()
	pod.EXPECT().SecretCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	sys.EXPECT().StartUnit(mock.Anything, mock.Anything).Return(nil).Maybe()

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		ListenAddr:   "127.0.0.1:0",
		SecretsDir:   t.TempDir(),
	}
	a := newTestAgent(t, cfg,
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(repoDir),
		WithStatePath(statePath),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.NoError(t, a.Run(ctx))
	store := state.NewStore(statePath)
	st, err := store.Load()
	require.NoError(t, err)
	require.NotEmpty(t, st.AppliedSHA, "the first commit applies: its mounted config is valid")
	require.Contains(t, st.ManagedFiles, "secret:picolet_system_config_next", "the next config is already managed")

	// Repoint only the mount; the managed secret stays unchanged (Noop).
	writeTestFile(t, repoDir, quadlet, `[Container]
Image=ghcr.io/schjan/picolet:latest
Secret=picolet_system_config_next,target=/etc/picolet/config.yml
`)
	clear(written)
	_, err = a.ReconcileOnce(t.Context(), "switch-sha", st, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no_such_key_ever")
	assert.Empty(t, written, "nothing is written")
	st2, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, st.AppliedSHA, st2.AppliedSHA)
}

// TestAgentRefusesDroppingMountedSelfConfig covers a commit that renames the
// Agent config secret's file (or otherwise stops resolving it) while the
// Agent quadlet keeps mounting the old secret: the apply would remove the
// secret the Agent's next start reads, so the reconciliation is refused and
// nothing is written or removed.
func TestAgentRefusesDroppingMountedSelfConfig(t *testing.T) {
	t.Parallel()
	const (
		oldConfig = "services/picolet-system/secrets/picolet_system_config.yml"
		newConfig = "services/picolet-system/secrets/picolet_cfg.yml"
	)
	bareDir, _ := initAgentBundleRepo(t, "hostname: test-host\nrepo_url: https://example.com/fleet.git\n")
	h := startSelfConfigAgent(t, bareDir)
	require.Contains(t, h.st.ManagedFiles, "secret:picolet_system_config", "the first commit renders the Agent config")

	// Rename the config's file: the Fleet now resolves picolet_cfg, while
	// the unit still mounts picolet_system_config.
	require.NoError(t, os.Remove(filepath.Join(h.repoDir, oldConfig)))
	writeTestFile(t, h.repoDir, newConfig, "hostname: test-host\nrepo_url: https://example.com/fleet.git\n")
	h.clearWrites()
	_, err := h.a.ReconcileOnce(t.Context(), "rename-sha", h.st, h.store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "picolet_system_config")
	assert.Contains(t, err.Error(), "/etc/picolet/config.yml")
	h.assertNothingWritten(t)
	st, err := h.store.Load()
	require.NoError(t, err)
	assert.Contains(t, st.ManagedFiles, "secret:picolet_system_config")
}

// selfConfigHarness is an Agent whose Fleet's first commit has applied,
// recording what the following reconciliations write.
type selfConfigHarness struct {
	a       *Agent
	repoDir string
	store   *state.Store
	st      *state.State // after the first commit
	written map[string][]byte
	created []string // secrets
	removed []string // secrets
}

// startSelfConfigAgent runs the Agent on the Fleet in bareDir until its first
// commit applies; the harness holds that commit's writes until clearWrites.
func startSelfConfigAgent(t *testing.T, bareDir string, opts ...Option) *selfConfigHarness {
	t.Helper()
	h := &selfConfigHarness{repoDir: filepath.Join(t.TempDir(), "clone")}
	statePath := filepath.Join(t.TempDir(), "state.json")
	sys, pod, fw := newBareMocks(t)
	h.written = setupApplyMocks(sys, pod, fw)
	pod.EXPECT().SecretExists(mock.Anything, mock.Anything).Return(false, nil).Maybe()
	pod.EXPECT().SecretCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name string, _ []byte, _ bool) error {
			h.created = append(h.created, name)
			return nil
		}).Maybe()
	pod.EXPECT().SecretRemove(mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, name string) error {
			h.removed = append(h.removed, name)
			return nil
		}).Maybe()
	sys.EXPECT().StartUnit(mock.Anything, mock.Anything).Return(nil).Maybe()

	cfg := &agentcfg.Config{
		Hostname:     "test-host",
		RepoURL:      bareDir,
		RepoBranch:   "master",
		PollInterval: time.Second,
		ListenAddr:   "127.0.0.1:0",
		SecretsDir:   t.TempDir(),
	}
	h.a = newTestAgent(t, cfg, append([]Option{
		WithSystemd(sys),
		WithPodman(pod),
		WithFileWriter(fw),
		WithRepoPath(h.repoDir),
		WithStatePath(statePath),
	}, opts...)...)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.NoError(t, h.a.Run(ctx))
	h.store = state.NewStore(statePath)
	var err error
	h.st, err = h.store.Load()
	require.NoError(t, err)
	require.NotEmpty(t, h.st.AppliedSHA, "the first commit applies")
	return h
}

// clearWrites forgets the writes recorded so far.
func (h *selfConfigHarness) clearWrites() {
	clear(h.written)
	h.created, h.removed = nil, nil
}

// assertNothingWritten checks that no reconciliation since clearWrites wrote
// a file or created or removed a secret, and that the applied SHA is still
// the first commit's.
func (h *selfConfigHarness) assertNothingWritten(t *testing.T) {
	t.Helper()
	assert.Empty(t, h.written, "nothing is written")
	assert.Empty(t, h.created, "no secret is created")
	assert.Empty(t, h.removed, "no secret is removed")
	st, err := h.store.Load()
	require.NoError(t, err)
	assert.Equal(t, h.st.AppliedSHA, st.AppliedSHA)
}

// TestAgentChecksSelfConfigCredentialsThroughNewUnitMounts covers a commit
// that changes the Agent quadlet's mounts: each credential file of the
// config the Agent's next start reads is looked up through the new unit's
// mounts. A file under a mount whose source the running container also
// mounts is checked where the running container sees it; a file the new
// unit no longer mounts, or mounts from a source the running container
// cannot see, is refused before anything is written.
func TestAgentChecksSelfConfigCredentialsThroughNewUnitMounts(t *testing.T) {
	t.Parallel()
	// seen is where the running container sees the Host's secrets dir
	// (/host/secrets); it holds the webhook secret.
	seen := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(seen, "hook"), []byte("secret\n"), 0o600))
	moved := filepath.Join(t.TempDir(), "not-mounted-here") // a target the running container lacks
	config := func(hookPath string) string {
		return "hostname: test-host\nrepo_url: https://example.com/fleet.git\nwebhook_secret_path: " + hookPath + "\n"
	}
	current := agentBundleUnit("Volume=/host/secrets:" + seen + ":ro\n")

	tests := []struct {
		name      string
		unit      string
		config    string
		wantError []string // empty: the commit applies
	}{
		{
			name:      "mount dropped",
			unit:      agentBundleUnit(""),
			config:    config(filepath.Join(seen, "hook")),
			wantError: []string{"webhook_secret_path", seen, "no longer mounts", "own commit"},
		},
		{
			name:      "mount repointed at another source",
			unit:      agentBundleUnit("Volume=/host/other:" + seen + ":ro\n"),
			config:    config(filepath.Join(seen, "hook")),
			wantError: []string{"webhook_secret_path", "/host/other", "own commit"},
		},
		{
			name:      "new mount and config in one commit",
			unit:      current + "Volume=/host/new:" + moved + ":ro\n",
			config:    config(filepath.Join(moved, "hook")),
			wantError: []string{"webhook_secret_path", "/host/new", "own commit"},
		},
		{
			name:      "same source at a new target, file missing",
			unit:      agentBundleUnit("Volume=/host/secrets/:" + moved + ":ro\n"),
			config:    config(filepath.Join(moved, "absent")),
			wantError: []string{"webhook_secret_path", filepath.Join(seen, "absent"), "not readable"},
		},
		{
			name:   "same source at a new target",
			unit:   agentBundleUnit("Volume=/host/secrets/:" + moved + ":ro\n"),
			config: config(filepath.Join(moved, "hook")),
		},
		{
			name:   "mount kept",
			unit:   current + "Volume=/host/data:/var/lib/picolet\n",
			config: config(filepath.Join(seen, "hook")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runMountChangeCase(t, current, config(filepath.Join(seen, "hook")), tt.unit, tt.config, tt.wantError)
		})
	}
}

// TestAgentChecksSelfConfigCredentialsThroughSecretMounts: a credential the
// Agent quadlet mounts as a Podman secret follows the secret, like a Volume=.
func TestAgentChecksSelfConfigCredentialsThroughSecretMounts(t *testing.T) {
	t.Parallel()
	seen := filepath.Join(t.TempDir(), "hook")
	require.NoError(t, os.WriteFile(seen, []byte("secret\n"), 0o600))
	moved := filepath.Join(t.TempDir(), "hook")
	config := func(hookPath string) string {
		return "hostname: test-host\nrepo_url: https://example.com/fleet.git\nwebhook_secret_path: " + hookPath + "\n"
	}
	current := agentBundleUnit("Secret=hook_secret,target=" + seen + "\n")
	runMountChangeCase(t, current, config(seen),
		agentBundleUnit("Secret=hook_secret,mode=0400,target="+moved+"\n"), config(moved), nil)
	runMountChangeCase(t, current, config(seen),
		agentBundleUnit("Secret=other_secret,target="+moved+"\n"), config(moved), []string{"secret other_secret", "own commit"})
}

// runMountChangeCase applies a first commit with the Agent quadlet unit and
// config, leaves unit on disk as the running Agent's, then reconciles a
// commit with nextUnit and nextConfig: applied when wantError is empty,
// otherwise refused before anything is written, with an error naming every
// wantError fragment.
func runMountChangeCase(t *testing.T, unit, config, nextUnit, nextConfig string, wantError []string) {
	t.Helper()
	const (
		quadlet    = "services/picolet-system/picolet-system.container"
		configFile = "services/picolet-system/secrets/picolet_system_config.yml"
	)
	bareDir, _ := initAgentBundleRepoWithUnit(t, unit, config)
	quadletDir := t.TempDir()
	h := startSelfConfigAgent(t, bareDir, WithQuadletDir(quadletDir))
	// The running Agent's container was started from the unit on disk; the
	// mocked file writer wrote nothing there.
	unitPath := filepath.Join(quadletDir, "picolet-system.container")
	require.Contains(t, h.written, unitPath)
	require.NoError(t, os.WriteFile(unitPath, h.written[unitPath], 0o600))

	writeTestFile(t, h.repoDir, quadlet, nextUnit)
	writeTestFile(t, h.repoDir, configFile, nextConfig)
	h.clearWrites()
	_, err := h.a.ReconcileOnce(t.Context(), "next-sha", h.st, h.store)
	if len(wantError) == 0 {
		require.NoError(t, err)
		st, err := h.store.Load()
		require.NoError(t, err)
		assert.Equal(t, "next-sha", st.AppliedSHA)
		return
	}
	require.Error(t, err)
	for _, want := range wantError {
		assert.Contains(t, err.Error(), want)
	}
	h.assertNothingWritten(t)
}

// A config secret the Agent quadlet mounts but the Fleet never managed is
// not Fleet-rendered: the guard leaves it alone. Once the Fleet managed it,
// a changeset removing it is refused.
func TestCheckSelfConfigUnresolvedMountedSecret(t *testing.T) {
	t.Parallel()
	unit := parser.NewUnitFile()
	require.NoError(t, unit.Parse(agentBundleUnit("")))
	resolved := &resolver.ResolvedHost{
		Host: &config.HostConfig{Hostname: "test-host"},
		Files: []resolver.ResolvedFile{{
			DestPath: "/etc/containers/systemd/picolet/picolet-system.container", Category: config.CategoryContainer,
			ServiceName: "picolet-system.service", ParsedUnit: unit,
		}},
	}
	noRead := func(string) ([]byte, error) { return nil, os.ErrNotExist }

	neverManaged := reconciler.Diff(resolved.Files, state.NewState())
	require.NoError(t, CheckSelfConfig(resolved, neverManaged, noRead))

	st := state.NewState()
	st.ManagedFiles["secret:picolet_system_config"] = state.ManagedFile{Hash: "h", Category: config.CategorySecret}
	err := CheckSelfConfig(resolved, reconciler.Diff(resolved.Files, st), noRead)
	require.ErrorContains(t, err, "refusing to remove the secret picolet_system_config")
}

// selfConfigChange is a commit reconciled against a state that manages the
// Agent quadlet (running, also on disk) and its config secret
// picolet_system_config, for calling CheckSelfConfig directly.
type selfConfigChange struct {
	running string  // the Agent quadlet on disk
	next    string  // the Agent quadlet the commit renders
	config  *string // the picolet_system_config content the commit renders; nil: not resolved
	files   []resolver.ResolvedFile
	managed []string // further dest paths the state manages
	// unchanged makes the state hold the Agent quadlet and config as the
	// commit renders them, so the commit writes neither.
	unchanged bool
}

const selfConfigUnitPath = "/etc/containers/systemd/picolet/picolet-system.container"

func (c selfConfigChange) check(t *testing.T) error {
	t.Helper()
	unit := parser.NewUnitFile()
	require.NoError(t, unit.Parse(c.next))
	files := []resolver.ResolvedFile{{
		DestPath: selfConfigUnitPath, Category: config.CategoryContainer,
		ServiceName: "picolet-system.service", ParsedUnit: unit, Content: c.next,
	}}
	if c.config != nil {
		files = append(files, resolver.ResolvedFile{DestPath: "secret:picolet_system_config", Category: config.CategorySecret, Content: *c.config})
	}
	files = append(files, c.files...)
	st := state.NewState()
	st.ManagedFiles[selfConfigUnitPath] = state.ManagedFile{Hash: "old", Category: config.CategoryContainer}
	st.ServiceNames[selfConfigUnitPath] = "picolet-system.service"
	st.ManagedFiles["secret:picolet_system_config"] = state.ManagedFile{Hash: "old", Category: config.CategorySecret}
	if c.unchanged {
		agentFiles := files[:len(files)-len(c.files)]
		reconciler.MergeChangeset(st, reconciler.Diff(agentFiles, state.NewState()))
	}
	for _, dest := range c.managed {
		category := config.CategorySecret
		if filepath.Ext(dest) == ".volume" {
			category = config.CategoryVolume
		}
		st.ManagedFiles[dest] = state.ManagedFile{Hash: "old", Category: category}
	}
	read := func(p string) ([]byte, error) {
		if p == selfConfigUnitPath {
			return []byte(c.running), nil
		}
		return nil, os.ErrNotExist
	}
	return CheckSelfConfig(&resolver.ResolvedHost{Host: &config.HostConfig{Hostname: "test-host"}, Files: files}, reconciler.Diff(files, st), read)
}

// assertRefusal checks a CheckSelfConfig result: nil when wantError is empty,
// otherwise an error naming every wantError fragment.
func assertRefusal(t *testing.T, err error, wantError []string) {
	t.Helper()
	if len(wantError) == 0 {
		require.NoError(t, err)
		return
	}
	require.Error(t, err)
	for _, want := range wantError {
		assert.Contains(t, err.Error(), want)
	}
}

// webhookConfig is a valid Agent config naming hookPath as a credential.
func webhookConfig(hookPath string) *string {
	return new("hostname: test-host\nrepo_url: https://example.com/fleet.git\nwebhook_secret_path: " + hookPath + "\n")
}

// A commit that changes the Agent quadlet's config mount, or drops it, while
// the running Agent reads a Fleet-managed config is refused when the Agent's
// next start would find no config: the mount names a secret the Fleet does
// not resolve (whether the old config file stays, is renamed or is dropped),
// or the new quadlet mounts nothing at the config path, or only a volume,
// tmpfs or other mount that is not a Host path. A Host file (or directory)
// bind-mounted there is a hand-managed config and passes.
func TestCheckSelfConfigRefusesLosingFleetManagedConfig(t *testing.T) {
	t.Parallel()
	valid := new("hostname: test-host\nrepo_url: https://example.com/fleet.git\n")
	renamed := resolver.ResolvedFile{DestPath: "secret:picolet_cfg", Category: config.CategorySecret, Content: *valid}
	tests := []struct {
		name      string
		change    selfConfigChange
		wantError []string // empty: passes
	}{
		{
			name:      "repointed at an unresolved secret, old config kept",
			change:    selfConfigChange{next: "[Container]\nImage=x\nSecret=picolet_typo,target=/etc/picolet/config.yml\n", config: valid},
			wantError: []string{"picolet_typo", "picolet_system_config", "/etc/picolet/config.yml"},
		},
		{
			name:      "repointed at an unresolved secret, old config renamed",
			change:    selfConfigChange{next: "[Container]\nImage=x\nSecret=picolet_typo,target=/etc/picolet/config.yml\n", files: []resolver.ResolvedFile{renamed}},
			wantError: []string{"picolet_typo", "picolet_system_config"},
		},
		{
			name:      "repointed at an unresolved secret, old config dropped",
			change:    selfConfigChange{next: "[Container]\nImage=x\nSecret=picolet_typo,target=/etc/picolet/config.yml\n"},
			wantError: []string{"picolet_typo"},
		},
		{
			name:      "mount line and config dropped",
			change:    selfConfigChange{next: "[Container]\nImage=x\n"},
			wantError: []string{"mounts nothing at /etc/picolet/config.yml", "picolet_system_config", "Volume="},
		},
		{
			name:      "mount line dropped, config kept",
			change:    selfConfigChange{next: "[Container]\nImage=x\n", config: valid},
			wantError: []string{"mounts nothing at /etc/picolet/config.yml"},
		},
		{
			name:      "tmpfs at the config directory",
			change:    selfConfigChange{next: "[Container]\nImage=x\nTmpfs=/etc/picolet\n"},
			wantError: []string{"mounts nothing at /etc/picolet/config.yml"},
		},
		{
			name:   "Host file mounted at the config path",
			change: selfConfigChange{next: "[Container]\nImage=x\nVolume=/srv/picolet/config.yml:/etc/picolet/config.yml:ro\n"},
		},
		{
			name:   "repointed at another resolved secret",
			change: selfConfigChange{next: "[Container]\nImage=x\nSecret=picolet_cfg,target=/etc/picolet/config.yml\n", files: []resolver.ResolvedFile{renamed}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.change.running = agentBundleUnit("")
			assertRefusal(t, tt.change.check(t), tt.wantError)
		})
	}
}

// A hand-managed config is a Host file (or directory) bind-mounted at the
// config path; a volume, .volume unit or glob mount there holds no config, so
// it is refused while the running Agent reads a Fleet-managed one.
func TestCheckSelfConfigHandManagedConfigIsHostPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		change    selfConfigChange
		wantError []string // empty: passes
	}{
		{
			name:   "Host file in the user's home mounted at the config path",
			change: selfConfigChange{next: "[Container]\nImage=x\nVolume=%h/.config/picolet/config.yml:/etc/picolet/config.yml:ro\n"},
		},
		{
			name:   "Host directory mounted over the config directory",
			change: selfConfigChange{next: "[Container]\nImage=x\nMount=type=bind,src=/srv/picolet,dst=/etc/picolet\n"},
		},
		{
			name:      "named volume over /etc",
			change:    selfConfigChange{next: "[Container]\nImage=x\nVolume=scratch:/etc\n"},
			wantError: []string{"scratch", "not a Host path", "picolet_system_config"},
		},
		{
			name:      "volume unit at the config directory",
			change:    selfConfigChange{next: "[Container]\nImage=x\nVolume=cfg.volume:/etc/picolet\n"},
			wantError: []string{"cfg.volume", "not a Host path"},
		},
		{
			name:      "glob mount at the config directory",
			change:    selfConfigChange{next: "[Container]\nImage=x\nMount=type=glob,src=/srv/*,dst=/etc/picolet\n"},
			wantError: []string{"not a Host path"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.change.running = agentBundleUnit("")
			assertRefusal(t, tt.change.check(t), tt.wantError)
		})
	}
}

// The config mount is found however its target is spelled: Podman mounts
// /etc/picolet//config.yml at the Agent config path too.
func TestCheckSelfConfigFindsNonCanonicalConfigMount(t *testing.T) {
	t.Parallel()
	bad := new("hostname: test-host\nrepo_url: https://example.com/fleet.git\nno_such_key_ever: 1\n")
	for _, target := range []string{"/etc/picolet//config.yml", "/etc/picolet/./config.yml", "/etc/picolet/sub/../config.yml"} {
		err := selfConfigChange{
			running: agentBundleUnit(""),
			next:    "[Container]\nImage=x\nSecret=picolet_system_config,target=" + target + "\n",
			config:  bad,
		}.check(t)
		require.ErrorContains(t, err, "no_such_key_ever", target)
	}
}

// Mounts that shadow a credential's directory, and mounts of a type the
// guard cannot compare beyond their spelling, are mounts too: a credential
// under one the running container does not have the same way is refused.
func TestCheckSelfConfigRefusesShadowedCredentials(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "hook"), []byte("x"), 0o600))
	hook := webhookConfig(filepath.Join(dir, "hook"))
	nested := webhookConfig(filepath.Join(dir, "sub", "hook"))
	volume := "Volume=/srv/s:" + dir + "\n"
	glob := "Mount=type=glob,src=/srv/s/*,dst=" + dir + "\n"
	tests := []struct {
		name      string
		running   string
		next      string
		config    *string
		wantError []string // empty: passes
	}{
		{name: "Tmpfs= over the mount", running: volume, next: volume + "Tmpfs=" + dir + "\n", config: hook, wantError: []string{dir, "mounts both"}},
		{name: "Mount=type=tmpfs over the mount", running: volume, next: volume + "Mount=type=tmpfs,destination=" + dir + "\n", config: hook, wantError: []string{dir, "mounts both"}},
		{name: "Tmpfs= below the mount", running: volume, next: volume + "Tmpfs=" + dir + "/sub\n", config: nested, wantError: []string{"own commit"}},
		{name: "glob mount repointed", running: glob, next: "Mount=type=glob,src=/srv/other/*,dst=" + dir + "\n", config: hook, wantError: []string{"/srv/other/*", "own commit"}},
		{name: "glob mount dropped", running: glob, next: "", config: hook, wantError: []string{"/srv/s/*", "no longer mounts"}},
		{name: "glob mount kept", running: glob, next: glob, config: hook},
		{
			name: "nested mount dropped", running: volume + "Volume=/srv/b:" + dir + "/sub\n", next: volume, config: nested,
			wantError: []string{"/srv/b", "own commit"},
		},
		{name: "nested mount kept", running: volume + "Volume=/srv/b:" + dir + "/sub\n", next: volume + "Volume=/srv/b:" + dir + "/sub\n", config: nested},
		{
			name: "volume subpath changed", running: "Mount=type=volume,source=creds,subpath=a,destination=" + dir + "\n",
			next: "Mount=type=volume,source=creds,subpath=b,destination=" + dir + "\n", config: hook, wantError: []string{"own commit"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertRefusal(t, selfConfigChange{running: agentBundleUnit(tt.running), next: agentBundleUnit(tt.next), config: tt.config}.check(t), tt.wantError)
		})
	}
}

// Mounts passed through PodmanArgs= are followed like Volume= ones: a
// credential whose PodmanArgs= mount the commit drops (or swaps for another
// option) is refused, as is one under a mount a PodmanArgs= option adds. A
// --volumes-from on either unit mounts what the guard cannot read, so every
// file credential is refused.
func TestCheckSelfConfigFollowsPodmanArgsMounts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	args := "PodmanArgs=--volume /srv/s:" + dir + "\n"
	from := "PodmanArgs=--volumes-from other\n"
	tests := []struct {
		name      string
		running   string
		next      string
		wantError []string // empty: passes
	}{
		{name: "PodmanArgs= mount dropped", running: args, next: "", wantError: []string{"/srv/s", "no longer mounts", "own commit"}},
		{
			name: "PodmanArgs= mount swapped for another option", running: args, next: "PodmanArgs=--security-opt apparmor=unconfined\n",
			wantError: []string{"/srv/s", "no longer mounts"},
		},
		{name: "PodmanArgs= mount added", running: "", next: args, wantError: []string{"/srv/s", "own commit"}},
		{name: "PodmanArgs= mount kept", running: args, next: args},
		{name: "PodmanArgs= mount moved to Volume=", running: args, next: "Volume=/srv/s:" + dir + "\n"},
		{name: "--volumes-from on both units", running: from, next: from, wantError: []string{"--volumes-from other", "PodmanArgs="}},
		{name: "--volumes-from dropped", running: from, next: "", wantError: []string{"--volumes-from other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := selfConfigChange{
				running: agentBundleUnit(tt.running), next: agentBundleUnit(tt.next), config: webhookConfig(filepath.Join(dir, "hook")),
			}.check(t)
			assertRefusal(t, err, tt.wantError)
		})
	}
}

// A credential whose mount source the same commit changes (the secret or the
// .volume unit it comes from) is refused: the running container shows its
// old content, which says nothing about the new one.
func TestCheckSelfConfigRefusesCredentialWhoseSourceChanges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	secretUnit := agentBundleUnit("Secret=hook_secret,target=" + filepath.Join(dir, "hook") + "\n")
	volumeUnit := agentBundleUnit("Volume=creds.volume:" + dir + "\n")
	const volumePath = "/etc/containers/systemd/picolet/creds.volume"
	tests := []struct {
		name      string
		change    selfConfigChange
		wantError []string // empty: passes
	}{
		{
			name: "secret rewritten",
			change: selfConfigChange{
				running: secretUnit, next: secretUnit, managed: []string{"secret:hook_secret"},
				files: []resolver.ResolvedFile{{DestPath: "secret:hook_secret", Category: config.CategorySecret, Content: "new"}},
			},
			wantError: []string{"secret hook_secret", "own commit"},
		},
		{
			name:      "secret removed",
			change:    selfConfigChange{running: secretUnit, next: secretUnit, managed: []string{"secret:hook_secret"}},
			wantError: []string{"secret hook_secret", "still mounts"},
		},
		{
			name: "volume unit rewritten",
			change: selfConfigChange{
				running: volumeUnit, next: volumeUnit, managed: []string{volumePath},
				files: []resolver.ResolvedFile{{DestPath: volumePath, Category: config.CategoryVolume, Content: "[Volume]\nDevice=/srv/other\n"}},
			},
			wantError: []string{"creds.volume", "own commit"},
		},
		{
			name:   "secret not managed",
			change: selfConfigChange{running: secretUnit, next: secretUnit},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.change.config = webhookConfig(filepath.Join(dir, "hook"))
			assertRefusal(t, tt.change.check(t), tt.wantError)
		})
	}
}

// A commit that removes a source the Agent quadlet mounts (a Podman secret,
// of either type, or a .volume unit) is refused even when it writes neither
// the Agent's config nor its quadlet: Podman could not create the Agent's
// container at its next start. A source the next quadlet no longer mounts
// may go.
func TestCheckSelfConfigRefusesRemovingMountedSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	hook := filepath.Join(dir, "hook")
	const volumePath = "/etc/containers/systemd/picolet/creds.volume"
	tests := []struct {
		name      string
		running   string // extra mount lines of the Agent quadlet on disk
		next      string // extra mount lines of the Agent quadlet the commit renders
		managed   []string
		wantError []string // empty: passes
	}{
		{
			name: "credential secret", running: "Secret=hook_secret,target=" + hook + "\n", managed: []string{"secret:hook_secret"},
			wantError: []string{"hook_secret", hook, "still mounts"},
		},
		{
			name: "secret spelled with source=", running: "Secret=source=hook_secret,target=" + hook + "\n", managed: []string{"secret:hook_secret"},
			wantError: []string{"secret hook_secret", "still mounts"},
		},
		{
			name: "secret not a credential", running: "Secret=other,target=/run/other\n", managed: []string{"secret:other"},
			wantError: []string{"other", "still mounts"},
		},
		{
			name: "env secret", running: "Secret=token,type=env,target=TOKEN\n", managed: []string{"secret:token"},
			wantError: []string{"token", "still mounts"},
		},
		{
			name: "volume unit of a Volume=", running: "Volume=creds.volume:" + dir + "\n", managed: []string{volumePath},
			wantError: []string{"creds.volume", "still mounts"},
		},
		{
			name: "volume unit of a Mount=", running: "Mount=type=volume,source=creds.volume,destination=" + dir + "\n", managed: []string{volumePath},
			wantError: []string{"creds.volume", "still mounts"},
		},
		{name: "secret not mounted", managed: []string{"secret:hook_secret"}},
		{name: "secret mounted, not managed", running: "Secret=hook_secret,target=" + hook + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			unit := agentBundleUnit(tt.running)
			err := selfConfigChange{
				running: unit, next: unit, config: webhookConfig(hook), managed: tt.managed, unchanged: true,
			}.check(t)
			assertRefusal(t, err, tt.wantError)
		})
	}

	t.Run("mount dropped in the same commit", func(t *testing.T) {
		t.Parallel()
		running := agentBundleUnit("Secret=other,target=/run/other\n")
		err := selfConfigChange{
			running: running, next: agentBundleUnit(""), config: webhookConfig(hook), managed: []string{"secret:other"},
		}.check(t)
		require.NoError(t, err)
	})
}

// Volume= options that change what the mount shows (subpath=, an overlay's
// upperdir=) are part of its source, like a Mount='s subpath: a credential
// under a Volume= whose such options change is refused. Options that only
// change access or labelling are not, and their order does not matter.
func TestCheckSelfConfigComparesVolumeOptions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	volume := func(options string) string { return "Volume=creds:" + dir + options + "\n" }
	tests := []struct {
		name      string
		running   string
		next      string
		wantError []string // empty: passes
	}{
		{name: "subpath changed", running: volume(":subpath=/a"), next: volume(":subpath=/b"), wantError: []string{"subpath", "own commit"}},
		{name: "subpath dropped", running: volume(":subpath=/a"), next: volume(""), wantError: []string{"own commit"}},
		{name: "subpath added", running: volume(":ro"), next: volume(":ro,subpath=a"), wantError: []string{"own commit"}},
		{
			name: "overlay upperdir changed", running: volume(":O,upperdir=/u1,workdir=/w"), next: volume(":O,upperdir=/u2,workdir=/w"),
			wantError: []string{"upperdir=/u2", "own commit"},
		},
		{name: "subpath spelled differently", running: volume(":subpath=/a/"), next: volume(":subpath=a")},
		{name: "access and label options changed", running: volume(":ro,z"), next: volume(":Z")},
		{name: "options reordered", running: volume(":U,ro,subpath=a"), next: volume(":subpath=a,ro,U")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := selfConfigChange{
				running: agentBundleUnit(tt.running), next: agentBundleUnit(tt.next), config: webhookConfig(filepath.Join(dir, "hook")),
			}.check(t)
			assertRefusal(t, err, tt.wantError)
		})
	}
}

// The running Agent's config secret is found however Podman reads its
// Secret= line: with a source= key, or quoted. A config the Agent could not
// start with is refused there too.
func TestCheckSelfConfigFindsConfigMountSpellings(t *testing.T) {
	t.Parallel()
	bad := new("hostname: test-host\nrepo_url: https://example.com/fleet.git\nno_such_key_ever: 1\n")
	for _, line := range []string{
		"Secret=source=picolet_system_config,target=/etc/picolet/config.yml",
		"Secret=target=/etc/picolet/config.yml,source=picolet_system_config",
		`Secret="picolet_system_config,target=/etc/picolet/config.yml"`,
	} {
		unit := "[Container]\nImage=x\n" + line + "\n"
		err := selfConfigChange{running: unit, next: unit, config: bad}.check(t)
		require.ErrorContains(t, err, "no_such_key_ever", line)
	}
}

// TestAgentRefusesRemovingMountedCredentialSecret runs the Agent loop on a
// Fleet whose Agent quadlet mounts a Fleet-managed secret as a credential: a
// commit that only stops resolving that secret, leaving the config and the
// quadlet as they are, is refused and removes nothing.
func TestAgentRefusesRemovingMountedCredentialSecret(t *testing.T) {
	t.Parallel()
	const hookFile = "services/picolet-system/secrets/hook_secret"
	hook := filepath.Join(t.TempDir(), "hook")
	require.NoError(t, os.WriteFile(hook, []byte("x"), 0o600))
	unit := agentBundleUnit("Secret=hook_secret,target=" + hook + "\n")
	bareDir, _ := initAgentBundleRepoWithUnit(t, unit,
		"hostname: test-host\nrepo_url: https://example.com/fleet.git\nwebhook_secret_path: "+hook+"\n")
	pushToTestRepo(t, bareDir, map[string]string{hookFile: "hook\n"})
	quadletDir := t.TempDir()
	h := startSelfConfigAgent(t, bareDir, WithQuadletDir(quadletDir))
	require.Contains(t, h.st.ManagedFiles, "secret:hook_secret", "the first commit manages the credential secret")
	unitPath := filepath.Join(quadletDir, "picolet-system.container")
	require.NoError(t, os.WriteFile(unitPath, h.written[unitPath], 0o600))

	require.NoError(t, os.Remove(filepath.Join(h.repoDir, hookFile)))
	h.clearWrites()
	_, err := h.a.ReconcileOnce(t.Context(), "drop-sha", h.st, h.store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hook_secret")
	assert.Contains(t, err.Error(), "still mounts")
	h.assertNothingWritten(t)
}
