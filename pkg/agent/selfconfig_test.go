package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
)

// agentBundleUnit is the test Agent quadlet with the extra [Container] lines
// mounts (Volume=/Secret= lines).
func agentBundleUnit(mounts string) string {
	return `[Container]
Image=ghcr.io/schjan/picolet:latest
Secret=picolet_system_config,target=/etc/picolet/config.yml
` + mounts
}

// agentBundleFiles is a Fleet whose rootful test-host runs its own Agent
// bundle (picolet-system) with the Agent quadlet unit and the bundle secret
// configName (picolet_system_config.yml, or a .tmpl of it) holding body.
func agentBundleFiles(unit, configName, body string) map[string]string {
	return map[string]string{
		"fleet.yml":                "images: {}\nports: {picolet_system_metrics: 9418}\n",
		"assignments.yml":          "base:\n  services:\n    - picolet-system\n",
		"hosts/test-host/host.yml": "hostname: test-host\nrole: server\nfeatures: []\n",
		"services/picolet-system/picolet-system.container": unit,
		"services/picolet-system/secrets/" + configName:    body,
	}
}

// mocksSelfConfig takes the calls of applying an Agent bundle: an apply's,
// plus its secret creates and unit starts.
const mocksSelfConfig = mocksApply | mockStarts | mockSecrets

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
			config:    *webhookConfig(missing),
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
	bareDir, headSHA := initFleetRepo(t, agentBundleFiles(agentBundleUnit(""), "picolet_system_config.yml", agentConfig))
	h := newLoopHarness(t, bareDir)
	h.cfg.PollInterval = 200 * time.Millisecond
	// Only the applying case may write; the strict mocks fail the refusing
	// cases on any write, on every tick until the failed-SHA gate closes.
	h.mocks = mocksNoop
	if len(wantError) == 0 {
		h.mocks = mocksSelfConfig
	}
	h.runUntil(t, func() bool {
		st, err := h.store.Load()
		return err == nil && (st.AppliedSHA != "" || st.FailedCount >= 3)
	})

	st, err := h.store.Load()
	require.NoError(t, err)
	if len(wantError) == 0 {
		assert.Equal(t, headSHA, st.AppliedSHA)
		assert.Empty(t, st.FailedSHA)
		assert.Contains(t, h.created, "picolet_system_config")
		assert.Contains(t, h.written, "/etc/containers/systemd/picolet/picolet-system.container")
		return
	}
	assert.Equal(t, headSHA, st.FailedSHA, "the reconciliation fails like any pre-apply failure")
	assert.Empty(t, st.AppliedSHA)
	assert.Empty(t, st.ManagedFiles, "nothing is written")

	_, err = h.a.ReconcileOnce(t.Context(), headSHA, state.NewState(), state.NewStore(filepath.Join(t.TempDir(), "state.json")))
	require.Error(t, err)
	for _, want := range wantError {
		assert.Contains(t, err.Error(), want)
	}
	h.assertNothingWritten(t)
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
	bareDir, _ := initFleetRepo(t, agentBundleFiles(agentBundleUnit(""), "picolet_system_config.yml", "hostname: test-host\nrepo_url: https://example.com/fleet.git\n"))
	pushToTestRepo(t, bareDir, map[string]string{
		nextConfig: "hostname: test-host\nrepo_url: https://example.com/fleet.git\nno_such_key_ever: x\n",
	})
	h := newLoopHarness(t, bareDir)
	h.mocks = mocksSelfConfig
	h.runUntilApplied(t)
	require.Contains(t, h.st.ManagedFiles, "secret:picolet_system_config_next", "the next config is already managed")

	// Repoint only the mount; the managed secret stays unchanged (Noop).
	writeTestFile(t, h.repoDir, quadlet, `[Container]
Image=ghcr.io/schjan/picolet:latest
Secret=picolet_system_config_next,target=/etc/picolet/config.yml
`)
	h.reset()
	_, err := h.a.ReconcileOnce(t.Context(), "switch-sha", h.st, h.store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no_such_key_ever")
	h.assertNothingWritten(t)
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
	bareDir, _ := initFleetRepo(t, agentBundleFiles(agentBundleUnit(""), "picolet_system_config.yml", "hostname: test-host\nrepo_url: https://example.com/fleet.git\n"))
	h := newLoopHarness(t, bareDir)
	h.mocks = mocksSelfConfig | mockSecretRemoves
	h.runUntilApplied(t)
	require.Contains(t, h.st.ManagedFiles, "secret:picolet_system_config", "the first commit renders the Agent config")

	// Rename the config's file: the Fleet now resolves picolet_cfg, while
	// the unit still mounts picolet_system_config.
	require.NoError(t, os.Remove(filepath.Join(h.repoDir, oldConfig)))
	writeTestFile(t, h.repoDir, newConfig, "hostname: test-host\nrepo_url: https://example.com/fleet.git\n")
	h.reset()
	_, err := h.a.ReconcileOnce(t.Context(), "rename-sha", h.st, h.store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "picolet_system_config")
	assert.Contains(t, err.Error(), "/etc/picolet/config.yml")
	h.assertNothingWritten(t)
	st, err := h.store.Load()
	require.NoError(t, err)
	assert.Contains(t, st.ManagedFiles, "secret:picolet_system_config")
}

// TestAgentFreezesMountLines runs the Agent loop over commits that change the
// Agent quadlet or its config with the quadlet on disk: a commit that changes
// a mount line is refused before anything is written, whatever else it
// changes; one that changes only the image, or only the config, is checked
// in the running container (credentials read as spelled) and applies.
func TestAgentFreezesMountLines(t *testing.T) {
	t.Parallel()
	// seen is where the running container sees the Host's secrets dir
	// (/host/secrets); it holds the webhook secret.
	seen := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(seen, "hook"), []byte("secret\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(seen, "token"), []byte("token\n"), 0o600))
	current := agentBundleUnit("Volume=/host/secrets:" + seen + ":ro\n")
	config := *webhookConfig(filepath.Join(seen, "hook"))
	frozen := []string{"changes its Volume= lines", "can't change through the Fleet", "on the Host", "commit the same lines"}

	tests := []struct {
		name      string
		unit      string
		config    string
		wantError []string // empty: the commit applies
	}{
		{name: "mount dropped", unit: agentBundleUnit(""), config: config, wantError: frozen},
		{name: "mount added with a config that uses it", unit: current + "Volume=/host/new:/srv/new:ro\n", config: *webhookConfig("/srv/new/hook"), wantError: frozen},
		{name: "image changed", unit: strings.Replace(current, ":latest", ":v2", 1), config: config},
		{name: "config changed", unit: current, config: config + "git_token_path: " + filepath.Join(seen, "token") + "\n"},
		{
			name: "config changed to a missing credential", unit: current, config: *webhookConfig(filepath.Join(seen, "absent")),
			wantError: []string{"webhook_secret_path", filepath.Join(seen, "absent"), "not readable"},
		},
		{name: "config changed to a relative credential", unit: current, config: *webhookConfig("hook"), wantError: []string{"webhook_secret_path", "relative", "absolute"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runMountChangeCase(t, current, config, tt.unit, tt.config, tt.wantError)
		})
	}
}

// runMountChangeCase applies a first commit with the Agent quadlet unit and
// config, leaves unit on disk as the running Agent's, then reconciles a
// commit with nextUnit and nextConfig (runAgentCommit).
func runMountChangeCase(t *testing.T, unit, config, nextUnit, nextConfig string, wantError []string) {
	t.Helper()
	runAgentCommit(t, agentBundleFiles(unit, "picolet_system_config.yml", config), map[string]string{
		"services/picolet-system/picolet-system.container":          nextUnit,
		"services/picolet-system/secrets/picolet_system_config.yml": nextConfig,
	}, wantError)
}

// runAgentCommit applies a first commit of the Fleet files, leaves its Agent
// quadlet on disk as the running Agent's, then reconciles a commit that
// writes next over them: applied when wantError is empty, otherwise refused
// before anything is written, with an error naming every wantError fragment.
func runAgentCommit(t *testing.T, files, next map[string]string, wantError []string) {
	t.Helper()
	bareDir, _ := initFleetRepo(t, files)
	quadletDir := t.TempDir()
	h := newLoopHarness(t, bareDir, WithQuadletDir(quadletDir))
	h.mocks = mocksSelfConfig | mockSecretRemoves
	h.runUntilApplied(t)
	// The running Agent's container was started from the unit on disk; the
	// mocked file writer wrote nothing there.
	unitPath := filepath.Join(quadletDir, "picolet-system.container")
	require.Contains(t, h.written, unitPath)
	require.NoError(t, os.WriteFile(unitPath, h.written[unitPath], 0o600))

	for name, content := range next {
		writeTestFile(t, h.repoDir, name, content)
	}
	h.reset()
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

// TestAgentRefusesSecondAgentService runs the Agent loop over commits that
// add a second unit for the Agent's service: another container quadlet with
// ServiceName=picolet-system (Quadlet writes the service once per quadlet,
// the last one wins), or a raw picolet-system.service, which systemd ranks
// above the generated one. Either would start the Agent from a unit the
// guard never checked, so the commit is refused before anything is written.
func TestAgentRefusesSecondAgentService(t *testing.T) {
	t.Parallel()
	files := agentBundleFiles(agentBundleUnit(""), "picolet_system_config.yml", "hostname: test-host\nrepo_url: https://example.com/fleet.git\n")
	for name, next := range map[string]map[string]string{
		"second container quadlet": {
			"services/picolet-system/zz-agent.container": "[Container]\nImage=ghcr.io/schjan/picolet:latest\nServiceName=picolet-system\n",
		},
		"raw unit": {
			"services/picolet-system/picolet-system.service": "[Unit]\nDescription=x\n\n[Service]\nExecStart=/usr/bin/podman run ghcr.io/schjan/picolet:latest\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runAgentCommit(t, files, next, []string{"picolet-system.service", "the Agent's", "one unit"})
		})
	}
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
	running string  // the Agent quadlet on disk (see noUnitOnDisk)
	next    string  // the Agent quadlet the commit renders
	config  *string // the picolet_system_config content the commit renders; nil: not resolved
	files   []resolver.ResolvedFile
	managed []string // further dest paths the state manages
	// unchanged makes the state hold the Agent quadlet and config as the
	// commit renders them, so the commit writes neither.
	unchanged bool
	// unchangedConfig makes the state hold the config as the commit renders
	// it, so the commit writes only the Agent quadlet.
	unchangedConfig bool
	// noUnitOnDisk leaves no Agent quadlet on disk: the running Agent was
	// not started from a Fleet unit.
	noUnitOnDisk bool
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
	if c.unchangedConfig && c.config != nil {
		reconciler.MergeChangeset(st, reconciler.Diff(files[1:2], state.NewState()))
	}
	for _, dest := range c.managed {
		category := config.CategorySecret
		if filepath.Ext(dest) == ".volume" {
			category = config.CategoryVolume
		}
		st.ManagedFiles[dest] = state.ManagedFile{Hash: "old", Category: category}
	}
	read := func(p string) ([]byte, error) {
		if p == selfConfigUnitPath && !c.noUnitOnDisk {
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

// changeCase is one CheckSelfConfig table row.
type changeCase struct {
	name      string
	change    selfConfigChange
	wantError []string // empty: passes
}

// runChangeCases checks each case's change, after defaults (when not nil)
// fills in what the table leaves out, in a parallel subtest.
func runChangeCases(t *testing.T, cases []changeCase, defaults func(*selfConfigChange)) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if defaults != nil {
				defaults(&tc.change)
			}
			assertRefusal(t, tc.change.check(t), tc.wantError)
		})
	}
}

// mountLines is the runChangeCases defaults for tables whose running and
// next hold only the Agent quadlet's extra mount lines: it wraps both in
// agentBundleUnit and sets config where a case leaves it nil.
func mountLines(config *string) func(*selfConfigChange) {
	return func(c *selfConfigChange) {
		c.running, c.next = agentBundleUnit(c.running), agentBundleUnit(c.next)
		if c.config == nil {
			c.config = config
		}
	}
}

// A commit that removes a source the Agent quadlet mounts (a Podman secret,
// of either type, or a .volume unit) is refused even when it writes neither
// the Agent's config nor its quadlet: Podman could not create the Agent's
// container at its next start. A source the next quadlet no longer mounts
// may go: the Host's operator drops the line on the Host first (the frozen
// mount lines), then commits the quadlet without it and the source's removal.
func TestCheckSelfConfigRefusesRemovingMountedSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	hook := filepath.Join(dir, "hook")
	const volumePath = "/etc/containers/systemd/picolet/creds.volume"
	// running holds the extra mount lines of the Agent quadlet on disk, which
	// the commit renders unchanged, with the config.
	runChangeCases(t, []changeCase{
		{
			name: "credential secret", change: selfConfigChange{running: "Secret=hook_secret,target=" + hook + "\n", managed: []string{"secret:hook_secret"}},
			wantError: []string{"hook_secret", hook, "still mounts"},
		},
		{
			name: "secret spelled with source=", change: selfConfigChange{running: "Secret=source=hook_secret,target=" + hook + "\n", managed: []string{"secret:hook_secret"}},
			wantError: []string{"secret hook_secret", "still mounts"},
		},
		{
			name: "secret not a credential", change: selfConfigChange{running: "Secret=other,target=/run/other\n", managed: []string{"secret:other"}},
			wantError: []string{"other", "still mounts"},
		},
		{
			name: "env secret", change: selfConfigChange{running: "Secret=token,type=env,target=TOKEN\n", managed: []string{"secret:token"}},
			wantError: []string{"token", "still mounts"},
		},
		{
			name: "volume unit of a Volume=", change: selfConfigChange{running: "Volume=creds.volume:" + dir + "\n", managed: []string{volumePath}},
			wantError: []string{"creds.volume", "still mounts"},
		},
		{
			name: "volume unit of a Mount=", change: selfConfigChange{running: "Mount=type=volume,source=creds.volume,destination=" + dir + "\n", managed: []string{volumePath}},
			wantError: []string{"creds.volume", "still mounts"},
		},
		{
			name: "volume unit of a Mount= without type=", change: selfConfigChange{running: "Mount=source=creds.volume,destination=" + dir + "\n", managed: []string{volumePath}},
			wantError: []string{"creds.volume", "still mounts"},
		},
		{name: "secret not mounted", change: selfConfigChange{managed: []string{"secret:hook_secret"}}},
		{name: "secret mounted, not managed", change: selfConfigChange{running: "Secret=hook_secret,target=" + hook + "\n"}},
	}, func(c *selfConfigChange) {
		c.next, c.unchanged = c.running, true
		mountLines(webhookConfig(hook))(c)
	})

	t.Run("mount dropped on the Host, then in the commit", func(t *testing.T) {
		t.Parallel()
		unit := agentBundleUnit("")
		err := selfConfigChange{
			running: unit, next: unit, config: webhookConfig(hook), unchangedConfig: true, managed: []string{"secret:other"},
		}.check(t)
		require.NoError(t, err)
	})
}

// A commit that writes the Agent's config or quadlet (its mount lines
// frozen) is refused when it also changes a Fleet-managed secret or
// .volume unit a credential is mounted from: the guard reads the credential
// as the running container shows it, the old content, while the restart
// after the apply reads the new one. A changed source that holds no
// credential passes.
func TestCheckSelfConfigRefusesChangedCredentialSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	hook := filepath.Join(dir, "hook")
	secretUnit := "Secret=hook_secret,target=" + hook + "\n"
	volumeUnit := "Volume=creds.volume:" + dir + "\n"
	const volumePath = "/etc/containers/systemd/picolet/creds.volume"
	newSecret := func(name string) []resolver.ResolvedFile {
		return []resolver.ResolvedFile{{DestPath: "secret:" + name, Category: config.CategorySecret, Content: "new"}}
	}
	newVolume := []resolver.ResolvedFile{{DestPath: volumePath, Category: config.CategoryVolume, Content: "[Volume]\nDevice=/srv/other\n"}}
	ownCommit := []string{"webhook_secret_path", "own commit"}
	runChangeCases(t, []changeCase{
		{
			name:      "config and the credential's secret",
			change:    selfConfigChange{running: secretUnit, next: secretUnit, managed: []string{"secret:hook_secret"}, files: newSecret("hook_secret")},
			wantError: append([]string{"the secret hook_secret, which this commit changes"}, ownCommit...),
		},
		{
			name:      "config and the credential's volume unit",
			change:    selfConfigChange{running: volumeUnit, next: volumeUnit, managed: []string{volumePath}, files: newVolume},
			wantError: append([]string{"the volume unit creds.volume, which this commit changes"}, ownCommit...),
		},
		{
			name: "quadlet and the credential's secret",
			change: selfConfigChange{
				running: secretUnit, next: secretUnit + "Pull=newer\n", unchangedConfig: true,
				managed: []string{"secret:hook_secret"}, files: newSecret("hook_secret"),
			},
			wantError: append([]string{"the secret hook_secret, which this commit changes"}, ownCommit...),
		},
		{
			name: "config and a secret holding no credential",
			change: selfConfigChange{
				running: "Secret=other,target=/run/other\n", next: "Secret=other,target=/run/other\n",
				managed: []string{"secret:other"}, files: newSecret("other"),
			},
		},
	}, mountLines(webhookConfig(hook)))
}

// An anonymous volume shares nothing across starts, however it is spelled:
// a Quadlet Volume= or podman -v/--volume with a target alone, or a Mount=
// or --mount of type volume (or no type) without a source, whatever its
// other options. A file credential it may hold is refused: the running
// container shows what it wrote there, and the next start finds the volume
// empty. (Quadlet itself
// refuses a sourceless Mount= volume; the guard refuses it all the same.)
func TestCheckSelfConfigRefusesCredentialUnderAnonymousVolume(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	spellings := map[string]string{
		"Volume= target alone":               "Volume=" + dir,
		"Volume= with an empty source":       "Volume=:" + dir + ":ro",
		"Mount= type=volume, target=":        "Mount=type=volume,target=" + dir,
		"Mount= type=volume, destination=":   "Mount=type=volume,destination=" + dir,
		"Mount= type=volume, dst=, options":  "Mount=type=volume,dst=" + dir + ",ro=true",
		"Mount= type=volume, empty source":   "Mount=type=volume,source=,target=" + dir,
		"Mount= without type":                "Mount=target=" + dir,
		"-v target alone":                    "PodmanArgs=-v " + dir,
		"--volume=target alone":              "PodmanArgs=--volume=" + dir,
		"--volume target alone":              "PodmanArgs=--volume " + dir,
		"--mount type=volume, target=":       "PodmanArgs=--mount type=volume,target=" + dir,
		"--mount=type=volume, target=":       "PodmanArgs=--mount=type=volume,target=" + dir,
		"--mount type=volume, destination=":  "PodmanArgs=--mount type=volume,destination=" + dir,
		"--mount=type=volume, dst=, options": "PodmanArgs=--mount=type=volume,dst=" + dir + ",ro",
		"--mount without type":               "PodmanArgs=--mount target=" + dir,
	}
	startsEmpty := []string{"webhook_secret_path", "anonymous volume at " + dir, "starts empty"}
	var cases []changeCase
	for name, line := range spellings {
		line += "\n"
		cases = append(cases, changeCase{name: name, change: selfConfigChange{running: line, next: line}, wantError: startsEmpty})
	}
	runChangeCases(t, cases, mountLines(webhookConfig(filepath.Join(dir, "hook"))))
}

// TestAgentRefusesConfigUnderAnonymousVolume runs the Agent loop: with the
// Agent quadlet mounting an anonymous volume over its credentials, a commit
// that changes only the config is refused with nothing written, since the
// next start finds the volume empty. (Quadlet refuses a Mount= volume
// without a source, so the Fleet passes it to podman run.)
func TestAgentRefusesConfigUnderAnonymousVolume(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token"), []byte("token\n"), 0o600))
	unit := agentBundleUnit("PodmanArgs=--mount=type=volume,target=" + dir + "\n")
	config := *webhookConfig(filepath.Join(dir, "hook"))
	runMountChangeCase(t, unit, config, unit, config+"git_token_path: "+filepath.Join(dir, "token")+"\n",
		[]string{"git_token_path", "anonymous volume at " + dir, "starts empty"})
}

// The running Agent's config secret is found however Podman reads its
// Secret= line: with a source= key, or quoted; and the new unit's config
// mount however its target is spelled: Podman mounts
// /etc/picolet//config.yml at the Agent config path too. A config the Agent
// could not start with is refused there too.
func TestCheckSelfConfigFindsConfigMountSpellings(t *testing.T) {
	t.Parallel()
	var cases []changeCase
	for _, line := range []string{
		"Secret=source=picolet_system_config,target=/etc/picolet/config.yml",
		"Secret=target=/etc/picolet/config.yml,source=picolet_system_config",
		`Secret="picolet_system_config,target=/etc/picolet/config.yml"`,
	} {
		unit := "[Container]\nImage=x\n" + line + "\n"
		cases = append(cases, changeCase{name: line, change: selfConfigChange{running: unit, next: unit}, wantError: []string{"no_such_key_ever"}})
	}
	for _, target := range []string{"/etc/picolet//config.yml", "/etc/picolet/./config.yml", "/etc/picolet/sub/../config.yml"} {
		unit := "[Container]\nImage=x\nSecret=picolet_system_config,target=" + target + "\n"
		cases = append(cases, changeCase{name: target, change: selfConfigChange{running: unit, next: unit}, wantError: []string{"no_such_key_ever"}})
	}
	runChangeCases(t, cases, func(c *selfConfigChange) {
		c.config = new("hostname: test-host\nrepo_url: https://example.com/fleet.git\nno_such_key_ever: 1\n")
	})
}

// TestAgentRefusesRemovingPodmanArgsSecret runs the Agent loop on a Fleet
// whose Agent quadlet passes a Fleet-managed secret through PodmanArgs=
// --secret, mounted or as an environment variable: a commit that only stops
// resolving that secret, leaving the config and the quadlet as they are, is
// refused and removes nothing, as for a Secret= line.
func TestAgentRefusesRemovingPodmanArgsSecret(t *testing.T) {
	t.Parallel()
	for name, line := range map[string]string{
		"mounted": "PodmanArgs=--secret token,target=/run/token\n",
		"env":     "PodmanArgs=--secret=token,type=env,target=TOKEN\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const tokenFile = "services/picolet-system/secrets/token"
			files := agentBundleFiles(agentBundleUnit(line), "picolet_system_config.yml",
				"hostname: test-host\nrepo_url: https://example.com/fleet.git\n")
			files[tokenFile] = "token\n"
			bareDir, _ := initFleetRepo(t, files)
			quadletDir := t.TempDir()
			h := newLoopHarness(t, bareDir, WithQuadletDir(quadletDir))
			h.mocks = mocksSelfConfig | mockSecretRemoves
			h.runUntilApplied(t)
			require.Contains(t, h.st.ManagedFiles, "secret:token", "the first commit manages the secret")
			unitPath := filepath.Join(quadletDir, "picolet-system.container")
			require.NoError(t, os.WriteFile(unitPath, h.written[unitPath], 0o600))

			require.NoError(t, os.Remove(filepath.Join(h.repoDir, tokenFile)))
			h.reset()
			_, err := h.a.ReconcileOnce(t.Context(), "drop-sha", h.st, h.store)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "refusing to remove the secret token")
			assert.Contains(t, err.Error(), "still mounts")
			assert.Contains(t, err.Error(), "PodmanArgs=--secret")
			h.assertNothingWritten(t)
		})
	}
}

// The Agent unit's mount, identity and access lines are frozen through the
// Fleet: a commit that writes the Agent quadlet with any of them changed from
// the unit on disk is refused, whatever else it changes, and names the
// Host-side procedure. Each row is a lockout an earlier guard modelled mount
// by mount; the frozen lines refuse all of them. A commit that changes only
// the image passes, as does one whose lines the Host's operator already put
// on disk by hand; a hand edit the commit would undo is refused. Without a
// unit on disk the lines are not compared.
func TestCheckSelfConfigFreezesMountLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	hook := filepath.Join(dir, "hook")
	volume := "Volume=/srv/s:" + dir + "\n"
	frozen := []string{"can't change through the Fleet", selfConfigUnitPath, "on the Host", "systemctl daemon-reload && systemctl restart picolet-system.service", "commit the same lines"}
	var cases []changeCase
	for name, next := range map[string]string{
		"mount dropped":                        "",
		"mount repointed":                      "Volume=/srv/other:" + dir + "\n",
		"mount options changed":                "Volume=/srv/s:" + dir + ":ro\n",
		"PodmanArgs= mount moved to Volume=":   "PodmanArgs=--volume /srv/s:" + dir + "\n",
		"unrelated mount added":                volume + "Volume=/srv/data:/var/lib/picolet\n",
		"Tmpfs= added":                         volume + "Tmpfs=" + dir + "\n",
		"VolatileTmp= added":                   volume + "VolatileTmp=true\n",
		"anonymous volume added":               volume + "Volume=/var/lib/picolet\n",
		"config mount switched":                "Secret=picolet_system_config_next,target=/etc/picolet/config.yml\n" + volume,
		"secret mount options changed":         volume + "Secret=hook,mode=0400,target=/run/hook\n",
		"Mask= over the config":                volume + "Mask=/etc/picolet\n",
		"Unmask=ALL added":                     volume + "Unmask=ALL\n",
		"--security-opt mask= over the config": volume + "PodmanArgs=--security-opt mask=/etc/picolet\n",
		"specifier target added":               volume + "Volume=/host/secrets:%h/x:ro\n",
		"relative target added":                volume + "Volume=/host/secrets:x\n",
		"--volumes-from added":                 volume + "PodmanArgs=--volumes-from=creds\n",
		"PodmanArgs= --user added":             volume + "PodmanArgs=-u 1000\n",
		"PodmanArgs= --read-only added":        volume + "PodmanArgs=--read-only\n",
		"User= added":                          volume + "User=1000\n",
		"UserNS= added":                        volume + "UserNS=keep-id\n",
		"ReadOnly= added":                      volume + "ReadOnly=true\n",
		"WorkingDir= added":                    volume + "WorkingDir=/srv\n",
		"GlobalArgs= added":                    volume + "GlobalArgs=--root=/srv/storage\n",
		"DropCapability= added":                volume + "DropCapability=all\n",
	} {
		cases = append(cases, changeCase{name: name, change: selfConfigChange{running: volume, next: next, unchangedConfig: true}, wantError: frozen})
	}
	added := volume + "Volume=/srv/data:/var/lib/picolet\n"
	cases = append(cases,
		changeCase{name: "config changed too", change: selfConfigChange{running: volume, next: added}, wantError: frozen},
		changeCase{name: "image changed", change: selfConfigChange{running: volume, next: volume + "Pull=newer\n", unchangedConfig: true}},
		changeCase{name: "lines of different keys reordered", change: selfConfigChange{running: volume + "Tmpfs=/run/x\n", next: "Tmpfs=/run/x\n" + volume, unchangedConfig: true}},
		changeCase{name: "lines edited on the Host first, then committed", change: selfConfigChange{running: added, next: added}},
		changeCase{
			name:      "lines edited on the Host, commit changes only the image",
			change:    selfConfigChange{running: added, next: volume + "Pull=newer\n", unchangedConfig: true},
			wantError: []string{"changes its Volume= lines", "can't change through the Fleet"},
		},
		changeCase{name: "no unit on disk", change: selfConfigChange{next: added + "Tmpfs=/run/x\nUser=1000\n", noUnitOnDisk: true}},
	)
	runChangeCases(t, cases, mountLines(webhookConfig(hook)))
}

// Beyond the mounts, the lines that decide what the Agent runs, where it
// reads its config and who runs podman are frozen too: Exec=, Entrypoint=,
// the container's environment files and Host environment, the PICOLET_CONFIG
// an Environment= line sets (unset is the default path), and every [Service]
// line but the restart and timeout settings, since systemd expands the
// service's environment in the generated command line and runs podman as
// its User=. Other Environment= variables stay changeable. The refusal
// names the file on disk and daemon-reload, without which Quadlet does not
// regenerate the service.
func TestCheckSelfConfigFreezesConfigAndServiceLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	volume := "Volume=/srv/s:" + dir + "\n"
	frozen := []string{"can't change through the Fleet", "edit " + selfConfigUnitPath + " on the Host", "systemctl daemon-reload && systemctl restart"}
	var cases []changeCase
	for name, next := range map[string]string{
		"Exec= added":                   "Exec=run --config /srv/picolet.yml\n",
		"Entrypoint= added":             "Entrypoint=/bin/sh\n",
		"EnvironmentFile= added":        "EnvironmentFile=/srv/picolet.env\n",
		"EnvironmentHost= added":        "EnvironmentHost=true\n",
		"PICOLET_CONFIG set elsewhere":  "Environment=PICOLET_CONFIG=/srv/picolet.yml\n",
		"PICOLET_CONFIG from podman":    "Environment=PICOLET_CONFIG\n",
		"[Service] Environment= added":  "[Service]\nEnvironment=CREDS=/srv/other\n",
		"[Service] User= added":         "[Service]\nUser=nobody\n",
		"[Service] ExecStartPre= added": "[Service]\nExecStartPre=/bin/true\n",
	} {
		cases = append(cases, changeCase{name: name, change: selfConfigChange{running: volume, next: volume + next, unchangedConfig: true}, wantError: frozen})
	}
	cases = append(cases,
		changeCase{name: "other Environment= changed", change: selfConfigChange{running: volume, next: volume + "Environment=TZ=UTC\n", unchangedConfig: true}},
		changeCase{
			name:   "PICOLET_CONFIG set to the default",
			change: selfConfigChange{running: volume, next: volume + "Environment=PICOLET_CONFIG=/etc/picolet/config.yml\n", unchangedConfig: true},
		},
		changeCase{
			name: "[Service] Restart= changed",
			change: selfConfigChange{
				running: volume + "[Service]\nRestart=always\nTimeoutStartSec=900\n", next: volume + "[Service]\nRestart=on-failure\n",
				unchangedConfig: true,
			},
		},
		changeCase{
			name:      "names what changed",
			change:    selfConfigChange{running: volume, next: volume + "Environment=PICOLET_CONFIG=/srv/picolet.yml\n[Service]\nUser=nobody\n", unchangedConfig: true},
			wantError: []string{"changes its Environment=PICOLET_CONFIG, [Service] User= lines"},
		},
	)
	runChangeCases(t, cases, mountLines(webhookConfig(filepath.Join(dir, "hook"))))

	t.Run("user Host", func(t *testing.T) {
		t.Parallel()
		running, next := parser.NewUnitFile(), parser.NewUnitFile()
		require.NoError(t, running.Parse(agentBundleUnit(volume)))
		require.NoError(t, next.Parse(agentBundleUnit("")))
		err := checkFrozenLines(running, next, "picolet.service", "/q/picolet.container", false)
		require.ErrorContains(t, err, "systemctl --user daemon-reload && systemctl --user restart picolet.service, as the Host's user")
	})
}

// A commit after which the Agent's next start would find no config, while
// the running Agent reads a Fleet-managed one, is refused: the config secret
// the frozen Secret= line keeps mounting stops being resolved (renamed or
// dropped, whether or not the commit writes the quadlet), or the commit
// repoints or drops that line. Without a unit on disk the running config is
// unknown, so a quadlet that mounts a Host file there instead passes.
func TestCheckSelfConfigRefusesLosingFleetManagedConfig(t *testing.T) {
	t.Parallel()
	valid := new("hostname: test-host\nrepo_url: https://example.com/fleet.git\n")
	renamed := []resolver.ResolvedFile{{DestPath: "secret:picolet_cfg", Category: config.CategorySecret, Content: *valid}}
	unit := agentBundleUnit("")
	lost := []string{"refusing to remove the secret picolet_system_config", "still mounts"}
	hostFile := "[Container]\nImage=x\nVolume=/srv/picolet/config.yml:/etc/picolet/config.yml:ro\n"
	runChangeCases(t, []changeCase{
		{name: "config renamed", change: selfConfigChange{running: unit, next: unit, files: renamed}, wantError: lost},
		{name: "config dropped", change: selfConfigChange{running: unit, next: unit}, wantError: lost},
		{name: "config dropped with an image change", change: selfConfigChange{running: unit, next: unit + "Pull=newer\n"}, wantError: lost},
		{
			name:      "mount repointed at the renamed config",
			change:    selfConfigChange{running: unit, next: "[Container]\nImage=x\nSecret=picolet_cfg,target=/etc/picolet/config.yml\n", files: renamed},
			wantError: []string{"changes its Secret= lines", "can't change through the Fleet"},
		},
		{name: "mount dropped", change: selfConfigChange{running: unit, next: hostFile, config: valid}, wantError: []string{"changes its Volume=, Secret= lines"}},
		{name: "Host file mounted there, no unit on disk", change: selfConfigChange{next: hostFile, noUnitOnDisk: true}},
	}, nil)
}

// With the mount lines frozen, the guard reads each credential in the
// running container, as spelled, and fails closed where a fresh start of the
// same unit may show it otherwise: a --volumes-from (the guard cannot read
// what it mounts), or a tmpfs (Tmpfs=, --tmpfs, VolatileTmp=, Mount=
// type=tmpfs) at the deepest mount holding the credential or at a target the
// guard cannot place (a specifier or relative target, which may hold any
// path): Quadlet's --rm container starts it empty. A mount at a specifier
// target from a Host path (the reference quadlet's %t/systemd) passes, as
// does a tmpfs a deeper Host mount covers.
func TestCheckSelfConfigFailsClosedWhereAFreshStartDiffers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "hook"), []byte("x"), 0o600))
	hook := filepath.Join(dir, "sub", "hook")
	startsEmpty := []string{"webhook_secret_path", "starts empty"}
	runtimeDir := "Volume=%t/systemd:%t/systemd\n"
	runChangeCases(t, []changeCase{
		{name: "--volumes-from", change: selfConfigChange{running: "PodmanArgs=--volumes-from=creds\n"}, wantError: []string{"webhook_secret_path", "--volumes-from creds"}},
		{name: "Tmpfs=", change: selfConfigChange{running: "Tmpfs=" + dir + "\n"}, wantError: startsEmpty},
		{name: "--tmpfs", change: selfConfigChange{running: "PodmanArgs=--tmpfs " + dir + "\n"}, wantError: startsEmpty},
		{name: "Mount= type=tmpfs", change: selfConfigChange{running: "Mount=type=tmpfs,dst=" + dir + "\n"}, wantError: startsEmpty},
		{
			name:      "VolatileTmp=",
			change:    selfConfigChange{running: "VolatileTmp=true\n", config: webhookConfig("/tmp/picolet-hook")},
			wantError: startsEmpty,
		},
		{name: "specifier tmpfs", change: selfConfigChange{running: "Tmpfs=%t/x\n"}, wantError: startsEmpty},
		{name: "relative tmpfs", change: selfConfigChange{running: "Tmpfs=x\n"}, wantError: startsEmpty},
		{name: "tmpfs with an image change", change: selfConfigChange{running: "Tmpfs=" + dir + "\n", next: "Tmpfs=" + dir + "\nPull=newer\n", unchangedConfig: true}, wantError: startsEmpty},
		{name: "tmpfs covered by a deeper Host mount", change: selfConfigChange{running: "Tmpfs=" + dir + "\nVolume=/srv/s:" + dir + "/sub\n"}},
		{name: "specifier Host path", change: selfConfigChange{running: runtimeDir}},
		{
			name:      "image mount",
			change:    selfConfigChange{running: "Mount=type=image,source=creds.build,destination=" + dir + "\n"},
			wantError: []string{"webhook_secret_path", "mounts an image at " + dir, "pull or rebuild"},
		},
		{
			name:      "--mount type=image",
			change:    selfConfigChange{running: "PodmanArgs=--mount type=image,src=creds:v1,dst=" + dir + "\n"},
			wantError: []string{"webhook_secret_path", "mounts an image at " + dir},
		},
		{
			name:      "credential in the image, Image= changed",
			change:    selfConfigChange{running: "Pull=newer\n", next: "Image=ghcr.io/schjan/picolet:v2\n"},
			wantError: []string{"webhook_secret_path", "under no mount", "Image="},
		},
		{name: "credential in the image, Image= kept", change: selfConfigChange{running: "Pull=newer\n"}},
		{name: "relative credential path", change: selfConfigChange{config: webhookConfig("hook")}, wantError: []string{"webhook_secret_path", "relative", "absolute"}},
	}, func(c *selfConfigChange) {
		if c.next == "" {
			c.next = c.running
		}
		mountLines(webhookConfig(hook))(c)
	})
}

// quadletFile is a resolved quadlet of category at dest, parsed from content.
func quadletFile(t *testing.T, dest string, category config.Category, content string) resolver.ResolvedFile {
	t.Helper()
	unit := parser.NewUnitFile()
	unit.Filename = filepath.Base(dest)
	require.NoError(t, unit.Parse(content))
	return resolver.ResolvedFile{
		DestPath: dest, Category: category, Content: content, ParsedUnit: unit,
		ServiceName: unitServiceNameForTest(t, unit),
	}
}

// unitServiceNameForTest is the service a parsed quadlet generates.
func unitServiceNameForTest(t *testing.T, unit *parser.UnitFile) string {
	t.Helper()
	name, err := quadlet.GetUnitServiceName(unit)
	require.NoError(t, err)
	return name + ".service"
}

// Exactly one Fleet unit may be the Agent's service: a second quadlet whose
// ServiceName= is the Agent's (of any type), a raw unit file of that name,
// or a unit whose [Install] Alias= names it would let systemd run the Agent
// from a unit the guard never checked, so every such changeset is refused,
// whether or not it writes the Agent's own files. A raw unit of another
// name passes.
func TestCheckSelfConfigRefusesSecondAgentService(t *testing.T) {
	t.Parallel()
	const quadletDir = "/etc/containers/systemd/picolet/"
	rawUnit := func(name, content string) []resolver.ResolvedFile {
		return []resolver.ResolvedFile{{
			DestPath: "/etc/systemd/system/" + name, Category: config.CategorySystemd, ServiceName: name,
			Content: config.PicoletMarker + "\n" + content,
		}}
	}
	quadlets := func(name string, category config.Category, content string) []resolver.ResolvedFile {
		return []resolver.ResolvedFile{quadletFile(t, quadletDir+name, category, content)}
	}
	second := []string{"picolet-system.service", "the Agent's service", "one unit"}
	runChangeCases(t, []changeCase{
		{
			name:      "container quadlet with ServiceName=",
			change:    selfConfigChange{files: quadlets("zz-agent.container", config.CategoryContainer, "[Container]\nImage=x\nServiceName=picolet-system\n")},
			wantError: append([]string{quadletDir + "zz-agent.container"}, second...),
		},
		{
			name:      "kube quadlet with ServiceName=",
			change:    selfConfigChange{files: quadlets("agent.kube", config.CategoryKube, "[Kube]\nYaml=agent.yml\nServiceName=picolet-system\n")},
			wantError: second,
		},
		{
			name:      "raw unit of that name",
			change:    selfConfigChange{files: rawUnit("picolet-system.service", "[Service]\nExecStart=/usr/bin/podman run x\n")},
			wantError: append([]string{"/etc/systemd/system/picolet-system.service"}, second...),
		},
		{
			name:      "raw unit aliased to it",
			change:    selfConfigChange{files: rawUnit("other.service", "[Service]\nExecStart=/bin/true\n\n[Install]\nAlias=picolet-system.service\n")},
			wantError: append([]string{"/etc/systemd/system/other.service", "Alias="}, second...),
		},
		{
			name:      "quadlet aliased to it",
			change:    selfConfigChange{files: quadlets("other.container", config.CategoryContainer, "[Container]\nImage=x\n\n[Install]\nAlias=picolet-system.service\n")},
			wantError: append([]string{"Alias="}, second...),
		},
		{name: "raw unit of another name", change: selfConfigChange{files: rawUnit("other.service", "[Service]\nExecStart=/bin/true\n")}},
	}, func(c *selfConfigChange) {
		c.running = agentBundleUnit("")
		c.next, c.config, c.unchanged = c.running, webhookConfig("/run/hook"), true
	})
}

// The running Agent's unit is the file on disk the state records as the
// Agent's service, whatever the commit renders there: when a commit moves
// ServiceName= of the Agent from the old quadlet to a new one (giving the old
// file another name, or deleting it), the new quadlet's frozen lines are
// compared with the old file's, and the refusal names the old file, the one
// on the Host to edit.
func TestCheckSelfConfigFindsRunningUnitByOldServiceName(t *testing.T) {
	t.Parallel()
	const newPath = "/etc/containers/systemd/picolet/agent.container"
	running := agentBundleUnit("Volume=/srv/s:/srv/creds\n")
	for name, keepOld := range map[string]bool{"old file renamed to another service": true, "old file deleted": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			valid := "hostname: test-host\nrepo_url: https://example.com/fleet.git\n"
			files := []resolver.ResolvedFile{
				quadletFile(t, newPath, config.CategoryContainer, agentBundleUnit("ServiceName=picolet-system\n")),
				{DestPath: "secret:picolet_system_config", Category: config.CategorySecret, Content: valid},
			}
			if keepOld {
				files = append(files, quadletFile(t, selfConfigUnitPath, config.CategoryContainer, running+"ServiceName=old-agent\n"))
			}
			st := state.NewState()
			st.ManagedFiles[selfConfigUnitPath] = state.ManagedFile{Hash: "old", Category: config.CategoryContainer}
			st.ServiceNames[selfConfigUnitPath] = "picolet-system.service"
			read := func(p string) ([]byte, error) {
				if p == selfConfigUnitPath {
					return []byte(running), nil
				}
				return nil, os.ErrNotExist
			}
			err := CheckSelfConfig(&resolver.ResolvedHost{Host: &config.HostConfig{Hostname: "test-host"}, Files: files}, reconciler.Diff(files, st), read)
			require.ErrorContains(t, err, "changes its Volume= lines")
			assert.Contains(t, err.Error(), "edit "+selfConfigUnitPath+" on the Host")
			assert.NotContains(t, err.Error(), newPath+" on the Host")
		})
	}
}

// The README lists every frozen key and every [Service] key the Fleet may
// change, so the docs cannot drift from the code.
func TestReadmeListsFrozenKeys(t *testing.T) {
	t.Parallel()
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)
	for _, key := range append(slices.Clone(frozenKeys), changeableServiceKeys...) {
		assert.True(t, strings.Contains(string(readme), "`"+key+"=`"), "README.md lists %s=", key) //nolint:testifylint // not the whole README in the failure
	}
}
