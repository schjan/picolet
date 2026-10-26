package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/systemd/parser"
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

// TestAgentChecksSelfConfigAgainstMountChanges runs the Agent loop over
// commits that change the Agent quadlet's mounts, its config, or both: the
// mounts change alone, while every credential stays under a mount the new
// unit keeps as it was; a mount change that drops or repoints a credential's
// mount, or comes with a config change, is refused before anything is
// written. A config change with the mounts unchanged is checked as spelled.
func TestAgentChecksSelfConfigAgainstMountChanges(t *testing.T) {
	t.Parallel()
	// seen is where the running container sees the Host's secrets dir
	// (/host/secrets); it holds the webhook secret.
	seen := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(seen, "hook"), []byte("secret\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(seen, "token"), []byte("token\n"), 0o600))
	moved := filepath.Join(t.TempDir(), "not-mounted-here") // a target the running container lacks
	current := agentBundleUnit("Volume=/host/secrets:" + seen + ":ro\n")
	config := *webhookConfig(filepath.Join(seen, "hook"))

	tests := []struct {
		name      string
		unit      string
		config    string
		wantError []string // empty: the commit applies
	}{
		{
			name:      "mount dropped",
			unit:      agentBundleUnit(""),
			config:    config,
			wantError: []string{"webhook_secret_path", "not under a mount the new unit keeps", "own commit"},
		},
		{
			name:      "mount repointed at another source",
			unit:      agentBundleUnit("Volume=/host/other:" + seen + ":ro\n"),
			config:    config,
			wantError: []string{"webhook_secret_path", "own commit"},
		},
		{
			name:      "new mount and config in one commit",
			unit:      current + "Volume=/host/new:" + moved + ":ro\n",
			config:    *webhookConfig(filepath.Join(moved, "hook")),
			wantError: []string{"changes its mounts and also its config", "own commit"},
		},
		{
			name:   "mount added, credentials kept",
			unit:   current + "Volume=/host/data:/var/lib/picolet\n",
			config: config,
		},
		{
			name:   "config changed, mounts kept",
			unit:   current,
			config: config + "git_token_path: " + filepath.Join(seen, "token") + "\n",
		},
		{
			name:      "config changed to a missing credential, mounts kept",
			unit:      current,
			config:    *webhookConfig(filepath.Join(seen, "absent")),
			wantError: []string{"webhook_secret_path", filepath.Join(seen, "absent"), "not readable"},
		},
		{
			name:      "config changed to a relative credential",
			unit:      current,
			config:    *webhookConfig("hook"),
			wantError: []string{"webhook_secret_path", "relative", "absolute"},
		},
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
// commit with nextUnit and nextConfig: applied when wantError is empty,
// otherwise refused before anything is written, with an error naming every
// wantError fragment.
func runMountChangeCase(t *testing.T, unit, config, nextUnit, nextConfig string, wantError []string) {
	t.Helper()
	const (
		quadlet    = "services/picolet-system/picolet-system.container"
		configFile = "services/picolet-system/secrets/picolet_system_config.yml"
	)
	bareDir, _ := initFleetRepo(t, agentBundleFiles(unit, "picolet_system_config.yml", config))
	quadletDir := t.TempDir()
	h := newLoopHarness(t, bareDir, WithQuadletDir(quadletDir))
	h.mocks = mocksSelfConfig | mockSecretRemoves
	h.runUntilApplied(t)
	// The running Agent's container was started from the unit on disk; the
	// mocked file writer wrote nothing there.
	unitPath := filepath.Join(quadletDir, "picolet-system.container")
	require.Contains(t, h.written, unitPath)
	require.NoError(t, os.WriteFile(unitPath, h.written[unitPath], 0o600))

	writeTestFile(t, h.repoDir, quadlet, nextUnit)
	writeTestFile(t, h.repoDir, configFile, nextConfig)
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
	// unchangedConfig makes the state hold the config as the commit renders
	// it, so the commit writes only the Agent quadlet.
	unchangedConfig bool
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

// runningAgentBundle is the runChangeCases defaults for tables whose cases
// change the default Agent quadlet on disk.
func runningAgentBundle(c *selfConfigChange) { c.running = agentBundleUnit("") }

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
	runChangeCases(t, []changeCase{
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
			name:      "repointed at another secret this commit creates",
			change:    selfConfigChange{next: "[Container]\nImage=x\nSecret=picolet_cfg,target=/etc/picolet/config.yml\n", files: []resolver.ResolvedFile{renamed}},
			wantError: []string{"its config secret:picolet_cfg", "own commit"},
		},
	}, runningAgentBundle)
}

// A hand-managed config is a Host file (or directory) bind-mounted at the
// config path; a volume, .volume unit or glob mount there holds no config, so
// it is refused while the running Agent reads a Fleet-managed one.
func TestCheckSelfConfigHandManagedConfigIsHostPath(t *testing.T) {
	t.Parallel()
	runChangeCases(t, []changeCase{
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
	}, runningAgentBundle)
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

	t.Run("mount dropped in the same commit", func(t *testing.T) {
		t.Parallel()
		running := agentBundleUnit("Secret=other,target=/run/other\n")
		err := selfConfigChange{
			running: running, next: agentBundleUnit(""), config: webhookConfig(hook), unchangedConfig: true, managed: []string{"secret:other"},
		}.check(t)
		require.NoError(t, err)
	})
}

// A commit that changes only the Agent quadlet's mount layout passes when
// every credential of the config lies under the same mounts (target and
// source as written) in both units, or under none; any other mount change
// around a credential is refused, as is a tmpfs kept there (it starts
// empty), a --volumes-from on either unit (the guard cannot read what it
// mounts) and a relative path.
func TestCheckSelfConfigRequiresKeptMounts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "hook"), []byte("x"), 0o600))
	nested := webhookConfig(filepath.Join(dir, "sub", "hook"))
	volume := "Volume=/srv/s:" + dir + "\n"
	refused := []string{"webhook_secret_path", "not under a mount the new unit keeps", "own commit"}
	runChangeCases(t, []changeCase{
		{name: "unrelated mount added", change: selfConfigChange{running: volume, next: volume + "Volume=/srv/data:/var/lib/picolet\n"}},
		{name: "image changed, mounts kept", change: selfConfigChange{running: volume, next: volume + "Pull=newer\n"}},
		{name: "credential in the image", change: selfConfigChange{running: "", next: "Volume=/srv/data:/var/lib/picolet\n"}},
		{name: "PodmanArgs= mount moved to Volume=", change: selfConfigChange{running: "PodmanArgs=--volume /srv/s:" + dir + "\n", next: volume}},
		{name: "mount dropped", change: selfConfigChange{running: volume, next: ""}, wantError: refused},
		{name: "mount added over the credential", change: selfConfigChange{running: "", next: volume}, wantError: refused},
		{name: "mount repointed", change: selfConfigChange{running: volume, next: "Volume=/srv/other:" + dir + "\n"}, wantError: refused},
		{name: "mount options changed", change: selfConfigChange{running: volume, next: "Volume=/srv/s:" + dir + ":ro\n"}, wantError: refused},
		{name: "Tmpfs= over the mount", change: selfConfigChange{running: volume, next: volume + "Tmpfs=" + dir + "\n"}, wantError: refused},
		{name: "Tmpfs= below the mount", change: selfConfigChange{running: volume, next: volume + "Tmpfs=" + dir + "/sub\n", config: nested}, wantError: refused},
		{
			name:      "nested mount dropped",
			change:    selfConfigChange{running: volume + "Volume=/srv/b:" + dir + "/sub\n", next: volume, config: nested},
			wantError: refused,
		},
		{
			name:      "tmpfs kept",
			change:    selfConfigChange{running: "Tmpfs=" + dir + "\n", next: "Tmpfs=" + dir + "\nVolume=/srv/data:/var/lib/picolet\n"},
			wantError: []string{"webhook_secret_path", "tmpfs or anonymous volume", "starts empty"},
		},
		{
			name:      "secret mount options changed",
			change:    selfConfigChange{running: "Secret=hook,target=" + dir + "/hook\n", next: "Secret=hook,mode=0400,target=" + dir + "/hook\n"},
			wantError: refused,
		},
		{
			name:      "--volumes-from",
			change:    selfConfigChange{running: volume + "PodmanArgs=--volumes-from other\n", next: volume},
			wantError: []string{"--volumes-from other", "own commit"},
		},
		{
			name:      "relative credential path",
			change:    selfConfigChange{running: volume, next: volume + "WorkingDir=" + dir + "\n", config: webhookConfig("hook")},
			wantError: []string{"webhook_secret_path", "relative", "absolute"},
		},
	}, func(c *selfConfigChange) {
		c.unchangedConfig = true
		mountLines(webhookConfig(filepath.Join(dir, "hook")))(c)
	})
}

// A commit that changes the Agent quadlet's mount layout and also something
// the Agent reads at start (its config, or a Fleet-managed secret or
// .volume unit the new quadlet uses) is refused: the running container shows
// their old content.
func TestCheckSelfConfigRefusesMountChangeWithWhatTheAgentReads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	secretUnit := "Secret=hook_secret,target=" + filepath.Join(dir, "hook") + "\n"
	volumeUnit := "Volume=creds.volume:" + dir + "\n"
	const volumePath = "/etc/containers/systemd/picolet/creds.volume"
	newSecret := []resolver.ResolvedFile{{DestPath: "secret:hook_secret", Category: config.CategorySecret, Content: "new"}}
	added := "Volume=/srv/data:/var/lib/picolet\n"
	runChangeCases(t, []changeCase{
		{
			name:      "config",
			change:    selfConfigChange{running: "", next: added},
			wantError: []string{"changes its mounts and also its config secret:picolet_system_config", "own commit"},
		},
		{
			name:      "mounted secret",
			change:    selfConfigChange{running: secretUnit, next: secretUnit + added, unchangedConfig: true, managed: []string{"secret:hook_secret"}, files: newSecret},
			wantError: []string{"the secret hook_secret", "own commit"},
		},
		{
			name: "volume unit",
			change: selfConfigChange{
				running: volumeUnit, next: volumeUnit + added, unchangedConfig: true, managed: []string{volumePath},
				files: []resolver.ResolvedFile{{DestPath: volumePath, Category: config.CategoryVolume, Content: "[Volume]\nDevice=/srv/other\n"}},
			},
			wantError: []string{"the volume unit creds.volume", "own commit"},
		},
	}, mountLines(webhookConfig(filepath.Join(dir, "hook"))))
}

// A commit that writes the Agent's config or quadlet with the mount layout
// unchanged is refused when it also changes a Fleet-managed secret or
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

// The guard fails closed on a mount it cannot read, whether or not the
// commit changes the mount lines: a --volumes-from (on either unit) or a
// tmpfs or anonymous volume that may hold a file credential refuses it, as
// does a mount whose target is relative or starts with a systemd specifier
// (%h, %t: the guard cannot tell which paths it holds) when the commit adds,
// drops or repoints one, or when it starts empty or its source changes. Such
// a mount kept as it was from a Host path (the reference quadlet's
// %t/systemd) passes.
func TestCheckSelfConfigFailsClosedOnMountsItCannotRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	hook := filepath.Join(dir, "hook")
	volumesFrom := "PodmanArgs=--volumes-from=creds\n"
	specifier := "Volume=/host/secrets:%h/x:ro\n"
	runtimeDir := "Volume=%t/systemd:%t/systemd\n"
	added := "Volume=/srv/data:/var/lib/picolet\n"
	cannotPlace := []string{"webhook_secret_path", "%h/x", "cannot tell which paths it holds", "own commit"}
	startsEmpty := []string{"webhook_secret_path", "starts empty"}
	runChangeCases(t, []changeCase{
		{
			name:      "--volumes-from, mount lines unchanged",
			change:    selfConfigChange{running: volumesFrom, next: volumesFrom},
			wantError: []string{"webhook_secret_path", "--volumes-from creds", "own commit"},
		},
		{name: "Tmpfs=, mount lines unchanged", change: selfConfigChange{running: "Tmpfs=" + dir + "\n", next: "Tmpfs=" + dir + "\n"}, wantError: startsEmpty},
		{name: "anonymous volume, mount lines unchanged", change: selfConfigChange{running: "Volume=" + dir + "\n", next: "Volume=" + dir + "\n"}, wantError: startsEmpty},
		{
			name:      "specifier target dropped",
			change:    selfConfigChange{running: specifier, next: "", unchangedConfig: true},
			wantError: cannotPlace,
		},
		{
			name:      "specifier target added",
			change:    selfConfigChange{running: "", next: specifier, unchangedConfig: true},
			wantError: cannotPlace,
		},
		{
			name:      "relative target added",
			change:    selfConfigChange{running: "", next: "Volume=/host/secrets:x\n", unchangedConfig: true},
			wantError: []string{"webhook_secret_path", "cannot tell which paths it holds", "own commit"},
		},
		{
			name:      "specifier tmpfs, mount lines unchanged",
			change:    selfConfigChange{running: "Tmpfs=%t/x\n", next: "Tmpfs=%t/x\n"},
			wantError: startsEmpty,
		},
		{
			name: "specifier secret target, secret changed",
			change: selfConfigChange{
				running: "Secret=hook_secret,target=%h/hook\n", next: "Secret=hook_secret,target=%h/hook\n",
				managed: []string{"secret:hook_secret"},
				files:   []resolver.ResolvedFile{{DestPath: "secret:hook_secret", Category: config.CategorySecret, Content: "new"}},
			},
			wantError: []string{"webhook_secret_path", "the secret hook_secret, which this commit changes"},
		},
		{name: "specifier Host path kept, config changed", change: selfConfigChange{running: runtimeDir, next: runtimeDir}},
		{name: "specifier Host path kept, mount added", change: selfConfigChange{running: runtimeDir, next: runtimeDir + added, unchangedConfig: true}},
	}, mountLines(webhookConfig(hook)))
}

// An anonymous volume shares nothing across starts, however it is spelled:
// a Quadlet Volume= or podman -v/--volume with a target alone, or a Mount=
// or --mount of type volume (or no type) without a source, whatever its
// other options. A file credential it may hold is refused, whether or not
// the commit changes the mount lines: the running container shows what it
// wrote there, and the next start finds the volume empty. (Quadlet itself
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
		cases = append(cases,
			changeCase{name: name + ", mount lines unchanged", change: selfConfigChange{running: line, next: line}, wantError: startsEmpty},
			changeCase{
				name:      name + ", mount added",
				change:    selfConfigChange{running: line, next: line + "Volume=/srv/data:/var/lib/picolet\n", unchangedConfig: true},
				wantError: startsEmpty,
			},
		)
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
	bareDir, _ := initFleetRepo(t, agentBundleFiles(unit, "picolet_system_config.yml", *webhookConfig(hook)))
	pushToTestRepo(t, bareDir, map[string]string{hookFile: "hook\n"})
	quadletDir := t.TempDir()
	h := newLoopHarness(t, bareDir, WithQuadletDir(quadletDir))
	h.mocks = mocksSelfConfig | mockSecretRemoves
	h.runUntilApplied(t)
	require.Contains(t, h.st.ManagedFiles, "secret:hook_secret", "the first commit manages the credential secret")
	unitPath := filepath.Join(quadletDir, "picolet-system.container")
	require.NoError(t, os.WriteFile(unitPath, h.written[unitPath], 0o600))

	require.NoError(t, os.Remove(filepath.Join(h.repoDir, hookFile)))
	h.reset()
	_, err := h.a.ReconcileOnce(t.Context(), "drop-sha", h.st, h.store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hook_secret")
	assert.Contains(t, err.Error(), "still mounts")
	h.assertNothingWritten(t)
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

// A commit that changes how the Agent's container shows a path other than
// by its mount lines fails closed on the file credentials it may hide: a
// PodmanArgs= option the guard cannot read (a --security-opt mask=, a user
// or user namespace, a read-only root or tmpfs, image volumes) added,
// dropped or changed refuses every file credential, and Mask=, Unmask= and
// VolatileTmp= count as mounts (a masked path, a tmpfs at /tmp). Such an
// option kept as it was (the reference quadlet's --security-opt
// apparmor=unconfined), or a PodmanArgs= option that shows no path, passes.
func TestCheckSelfConfigFailsClosedOnPathOptions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook"), []byte("x"), 0o600))
	hook := filepath.Join(dir, "hook")
	volume := "Volume=/srv/s:" + dir + "\n"
	apparmor := "PodmanArgs=--security-opt apparmor=unconfined\n"
	cannotRead := []string{"webhook_secret_path", "PodmanArgs=", "cannot check"}
	runChangeCases(t, []changeCase{
		{
			name:      "--security-opt=mask= added",
			change:    selfConfigChange{running: volume, next: volume + "PodmanArgs=--security-opt=mask=" + hook + "\n"},
			wantError: append([]string{"--security-opt mask=" + hook}, cannotRead...),
		},
		{
			name:      "--security-opt mask= added as two arguments",
			change:    selfConfigChange{running: volume, next: volume + "PodmanArgs=--security-opt mask=" + hook + "\n"},
			wantError: cannotRead,
		},
		{name: "-u added", change: selfConfigChange{running: volume, next: volume + "PodmanArgs=-u 1000\n"}, wantError: append([]string{"--user 1000"}, cannotRead...)},
		{name: "--userns changed", change: selfConfigChange{running: volume + "PodmanArgs=--userns=keep-id\n", next: volume + "PodmanArgs=--userns=auto\n"}, wantError: cannotRead},
		{name: "--read-only added", change: selfConfigChange{running: volume, next: volume + "PodmanArgs=--read-only\n"}, wantError: append([]string{"--read-only"}, cannotRead...)},
		{name: "--image-volume added", change: selfConfigChange{running: volume, next: volume + "PodmanArgs=--image-volume tmpfs\n"}, wantError: cannotRead},
		{name: "option kept, mount added", change: selfConfigChange{running: volume + apparmor, next: volume + apparmor + "Volume=/srv/data:/var/lib/picolet\n"}},
		{name: "--network added", change: selfConfigChange{running: volume, next: volume + "PodmanArgs=--network=host\n"}},
		{
			name:      "Mask= added over the credential",
			change:    selfConfigChange{running: volume, next: volume + "Mask=/proc/acpi:" + hook + "\n"},
			wantError: []string{"webhook_secret_path", "not under a mount the new unit keeps", "own commit"},
		},
		{name: "Mask= added elsewhere", change: selfConfigChange{running: volume, next: volume + "Mask=/proc/acpi\n"}},
		{
			name:      "Unmask=ALL added",
			change:    selfConfigChange{running: volume, next: volume + "Unmask=ALL\n"},
			wantError: []string{"webhook_secret_path", "cannot tell which paths it holds"},
		},
		{
			name:      "VolatileTmp= added",
			change:    selfConfigChange{running: "", next: "VolatileTmp=true\n", config: webhookConfig("/tmp/picolet-hook")},
			wantError: []string{"webhook_secret_path", "not under a mount the new unit keeps"},
		},
	}, func(c *selfConfigChange) {
		c.unchangedConfig = true
		mountLines(webhookConfig(hook))(c)
	})
}
