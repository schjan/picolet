package picolet_test

import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/validator"
)

const testdataDir = "testdata/example-fleet"

func TestIntegrationValidate(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)

	r, err := resolver.NewKeyless(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	require.NoError(t, validator.ValidateAll(t.Context(), repoFS, r, cfg, validator.AllowTestRecipients()))
}

// validate renders each Host's Agent bundle strictly, as bootstrap does: a
// provider ref the bundle needs at bootstrap fails CI, not the next
// `bootstrap machine`. The rootful Host's bundle is picolet-system, a Host
// with user:'s is picolet.
func TestIntegrationValidateAgentBundleStrict(t *testing.T) {
	t.Parallel()
	const fix = "use a YAML-value reference and let the agent resolve it at runtime"
	const sopsFix = "deliver the value as a --secrets-dir file and point the Agent config at its path; " +
		"credentials needed to fetch the Fleet can't be SOPS values"
	tests := []struct {
		name, user, service, fn, ref, fix string
	}{
		{name: "rootful readOpSecret", service: "picolet-system", fn: "readOpSecret", ref: "op://vault/item/token", fix: fix},
		{name: "rootful readProtonPassSecret", service: "picolet-system", fn: "readProtonPassSecret", ref: "pass://share/item/token", fix: fix},
		{name: "user readOpSecret", user: "app", service: "picolet", fn: "readOpSecret", ref: "op://vault/item/token", fix: fix},
		{name: "rootful readSopsSecret", service: "picolet-system", fn: "readSopsSecret", ref: "sops://secrets/git.sops.yaml#/token", fix: sopsFix},
		{name: "rootful readSopsData", service: "picolet-system", fn: "readSopsData", ref: "sops://secrets/git.sops.yaml#/git", fix: sopsFix},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := fmt.Sprintf(`{{ $ref := %q }}git_token: {{ %s $ref }}`, tt.ref, tt.fn)
			err := validateFleet(t, agentBundleFleet(tt.user, tt.service, true, body))
			require.Error(t, err)
			for _, want := range []string{"host test-host", "Agent bundle " + tt.service, fmt.Sprintf("%s %q", tt.fn, tt.ref), tt.fix} {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

// A Host without its Agent bundle assigned is skipped: the bundle's provider
// ref is not rendered, so validate passes.
func TestIntegrationValidateAgentBundleUnassignedSkipped(t *testing.T) {
	t.Parallel()
	body := `{{ $ref := "op://vault/item/token" }}git_token: {{ readOpSecret $ref }}`
	require.NoError(t, validateFleet(t, agentBundleFleet("", "picolet-system", false, body)))
}

// Secret References as plain YAML values are the Agent's to resolve at
// runtime: the Agent-bundle pass does not flag them.
func TestIntegrationValidateAgentBundleYAMLValueRefs(t *testing.T) {
	t.Parallel()
	require.NoError(t, validateFleet(t, agentBundleFleet("", "picolet-system", true, `onepassword:
  token_path: /etc/picolet/secrets/op-token
  git_token_ref: op://vault/item/token
  github_app_id_ref: op://vault/app/id
  github_installation_id_ref: op://vault/app/installation
  github_private_key_ref: op://vault/app/key
protonpass:
  git_token_ref: pass://share/item/token
  github_app_id_ref: pass://share/app/id
  github_installation_id_ref: pass://share/app/installation
  github_private_key_ref: pass://share/app/key`)))
}

// An Agent bundle whose quadlet mounts a config secret at
// /etc/picolet/config.yml that the bundle does not resolve (a renamed or
// dropped file) fails validate: bootstrap and the Agent's next start would
// find no config.
func TestIntegrationValidateAgentBundleUnresolvedConfigMount(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ user, service string }{{"", "picolet-system"}, {"app", "picolet"}} {
		t.Run(tt.service, func(t *testing.T) {
			t.Parallel()
			fleet := agentBundleFleet(tt.user, tt.service, true, "")
			secret := strings.ReplaceAll(tt.service, "-", "_") + "_config"
			cfgFile := "services/" + tt.service + "/secrets/" + secret + ".yml.tmpl"
			fleet["services/"+tt.service+"/secrets/renamed.yml.tmpl"] = fleet[cfgFile]
			delete(fleet, cfgFile)

			err := validateFleet(t, fleet)
			require.Error(t, err)
			for _, want := range []string{"host test-host", "Agent bundle " + tt.service, secret, "/etc/picolet/config.yml"} {
				require.ErrorContains(t, err, want)
			}

			// Podman mounts a non-canonical spelling of the target at the
			// config path too.
			unitFile := "services/" + tt.service + "/" + tt.service + ".container.tmpl"
			fleet[unitFile] = &fstest.MapFile{Data: []byte(strings.Replace(string(fleet[unitFile].Data),
				"target=/etc/picolet/config.yml", "target=/etc/picolet//config.yml", 1))}
			require.ErrorContains(t, validateFleet(t, fleet), secret)
		})
	}
}

// agentBundleFleet is a one-host Fleet whose Host test-host (user: user, or
// rootful when empty) carries the Agent bundle service, assigned to it when
// assign; configBody is appended to the bundle's Agent config template.
func agentBundleFleet(user, service string, assign bool, configBody string) fstest.MapFS {
	hostYML := "hostname: test-host\nrole: node\nfeatures: []\n"
	if user != "" {
		hostYML += "user: " + user + "\n"
	}
	assigned := ""
	if assign {
		assigned = service
	}
	secret := strings.ReplaceAll(service, "-", "_") + "_config"
	return fstest.MapFS{
		"fleet.yml":                &fstest.MapFile{Data: []byte("images: {picolet: \"picolet:test\"}\nports: {picolet_metrics: 9417, picolet_system_metrics: 9418}\n")},
		"assignments.yml":          &fstest.MapFile{Data: []byte("base: {}\nroles:\n  node:\n    services: [" + assigned + "]\nfeatures: {}\n")},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(hostYML)},
		"services/" + service + "/" + service + ".container.tmpl": &fstest.MapFile{Data: []byte(`[Container]
Image={{ index .Images "picolet" }}
Secret=` + secret + `,target=/etc/picolet/config.yml
`)},
		"services/" + service + "/secrets/" + secret + ".yml.tmpl": &fstest.MapFile{Data: []byte(`hostname: "{{ .Host.Hostname }}"
repo_url: "https://github.com/example/fleet.git"
` + configBody + "\n")},
	}
}

// validateFleet runs `picolet validate` on the Fleet in repoFS. Test Fleets
// declare the corpus's published TEST-ONLY recipients, so their warning is
// off; opts are extra validate options.
func validateFleet(t *testing.T, repoFS fs.FS, opts ...validator.Option) error {
	t.Helper()
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)
	r, err := resolver.NewKeyless(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	return validator.ValidateAll(t.Context(), repoFS, r, cfg, append([]validator.Option{validator.AllowTestRecipients()}, opts...)...)
}

// validate checks every readSopsSecret ref keyless and fails, naming the
// ref, on what it can tell is wrong without a key.
func TestIntegrationValidateSopsRefs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, ref, want string
		recipients      []string // test-host's age_recipients:; nil means srv-1
	}{
		{name: "valid", ref: "sops://secrets/db.sops.yaml#/db/password"},
		{name: "two-key rotation", ref: "sops://secrets/db.sops.yaml#/db/password", recipients: []string{"srv-3", "srv-2"}},
		{name: "key groups: threshold reached", ref: "sops://secrets/groups.sops.yaml#/db/password", recipients: []string{"srv-2", "srv-pq"}},
		{
			name: "no age_recipients", ref: "sops://secrets/db.sops.yaml#/db/password", recipients: []string{},
			want: "Host test-host references sops://secrets/db.sops.yaml#/db/password but declares no age_recipients",
		},
		{
			name: "not a recipient", ref: "sops://secrets/db.sops.yaml#/db/password", recipients: []string{"srv-3"},
			want: "secrets/db.sops.yaml: not a recipient: the file is not encrypted to Host test-host (Host test-host's recipients: " +
				testsops.Recipient(t, "srv-3") + "); add one of them to the file's .sops.yaml creation rule and run `sops updatekeys secrets/db.sops.yaml`",
		},
		{
			name: "key groups: too few", ref: "sops://secrets/groups.sops.yaml#/db/password", recipients: []string{"srv-1"},
			want: "the file is encrypted to Host test-host in 1 of the 2 key groups it needs",
		},
		{name: "missing file", ref: "sops://secrets/gone.sops.yaml#/db/password", want: "file does not exist"},
		{name: "no sops infix", ref: "sops://secrets/db.yaml#/db/password", want: "not a SOPS path"},
		{name: "dot-dot", ref: "sops://secrets/../secrets/db.sops.yaml#/db/password", want: "clean relative path"},
		{name: "not a SOPS file", ref: "sops://secrets/plain.sops.yaml#/db/password", want: "not a SOPS file"},
		{name: "missing path", ref: "sops://secrets/db.sops.yaml#/db/nope", want: "bad pointer"},
		{name: "not a leaf", ref: "sops://secrets/db.sops.yaml#/db", want: "not a leaf"},
		// The Agent decrypts a leaf the file's rules leave in plaintext, so
		// validate passes it too.
		{name: "unencrypted leaf", ref: "sops://secrets/db.sops.yaml#/note_unencrypted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			recipients := tt.recipients
			if recipients == nil {
				recipients = []string{"srv-1"}
			}
			err := validateFleet(t, sopsRefsFleet(t, tt.ref, recipients))
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.ErrorContains(t, err, tt.ref)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

// sopsRefsFleet is TestIntegrationValidateSopsRefs's Fleet: test-host,
// declaring recipients, renders ref through readSopsSecret.
func sopsRefsFleet(t *testing.T, ref string, recipients []string) fstest.MapFS {
	t.Helper()
	values := testsops.CorpusFile(t, "values.sops.yaml") // srv-1, srv-2, operator-1
	fleet := fstest.MapFS{
		"fleet.yml":                &fstest.MapFile{Data: []byte("images: {}\nports: {picolet_system_metrics: 9418}\n")},
		"assignments.yml":          &fstest.MapFile{Data: []byte("base:\n  secrets: [secrets/app.yml.tmpl]\nroles: {}\nfeatures: {}\n")},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(sopsHostYML(t, recipients))},
		"secrets/app.yml.tmpl":     &fstest.MapFile{Data: []byte(`password: {{ readSopsSecret "` + ref + `" }}`)},
		"secrets/db.sops.yaml":     &fstest.MapFile{Data: values},
		"secrets/db.yaml":          &fstest.MapFile{Data: values},
		config.OperatorsFile:       corpusOperatorsExcept(t, recipients),
	}
	if strings.Contains(ref, "groups.sops.yaml") {
		// An unreferenced file allows Operators only (the audience pass),
		// so the Fleet holds just the SOPS file the ref names.
		delete(fleet, "secrets/db.sops.yaml")
		// [srv-1 operator-1] [srv-2] [srv-pq], threshold 2
		fleet["secrets/groups.sops.yaml"] = &fstest.MapFile{Data: testsops.CorpusFile(t, "key-groups.sops.yaml")}
	}
	if strings.Contains(ref, "plain.sops.yaml") {
		// Only here: the encrypted-file pass fails on it whether referenced or not.
		fleet["secrets/plain.sops.yaml"] = &fstest.MapFile{Data: testsops.CorpusFile(t, "plaintext.sops.yaml")}
	}
	return fleet
}

// validate runs the keyless per-ref checks on a direct sops:// secrets:
// entry too: the recipient checks, the leaf rules and the non-empty check.
func TestIntegrationValidateDirectSopsRefs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, ref, want string
		recipients      []string // test-host's age_recipients:; nil means srv-1
	}{
		{name: "valid", ref: "sops://secrets/db.sops.yaml#/db/password"},
		{
			name: "no age_recipients", ref: "sops://secrets/db.sops.yaml#/db/password", recipients: []string{},
			want: "Host test-host references sops://secrets/db.sops.yaml#/db/password but declares no age_recipients",
		},
		{
			name: "drift", ref: "sops://secrets/db.sops.yaml#/db/password", recipients: []string{"srv-3"},
			want: "secrets/db.sops.yaml: not a recipient: the file is not encrypted to Host test-host",
		},
		{name: "not a leaf", ref: "sops://secrets/db.sops.yaml#/db", want: "not a leaf"},
		{name: "empty leaf", ref: "sops://secrets/db.sops.yaml#/db/empty", want: "a direct secrets: entry needs a non-empty value"},
		{name: "unencrypted leaf", ref: "sops://secrets/db.sops.yaml#/note_unencrypted"},
		{name: "invalid segment", ref: "sops://secrets/db.sops.yaml#/odd_keys/m~0n", want: "can't appear in a Podman secret name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			recipients := tt.recipients
			if recipients == nil {
				recipients = []string{"srv-1"}
			}
			err := validateFleet(t, fstest.MapFS{
				"fleet.yml":                &fstest.MapFile{Data: []byte("images: {}\nports: {picolet_system_metrics: 9418}\n")},
				"assignments.yml":          &fstest.MapFile{Data: []byte("base:\n  secrets: [\"" + tt.ref + "\"]\nroles: {}\nfeatures: {}\n")},
				"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(sopsHostYML(t, recipients))},
				"secrets/db.sops.yaml":     &fstest.MapFile{Data: testsops.CorpusFile(t, "values.sops.yaml")},
				config.OperatorsFile:       corpusOperatorsExcept(t, recipients),
			})
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.ref)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestIntegrationResolveGolden(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)

	r, err := resolver.NewKeyless(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	g := goldie.New(t, goldie.WithFixtureDir("testdata/fixtures"))

	for _, hostname := range cfg.SortedHostnames() {
		t.Run(hostname, func(t *testing.T) {
			t.Parallel()
			resolved, err := r.ResolveHost(t.Context(), hostname)
			require.NoError(t, err)

			for _, f := range resolved.Files {
				name := hostname + "/" + sanitizePath(f.DestPath)
				g.Assert(t, name, []byte(f.Content))
			}
		})
	}
}

// sanitizePath converts a dest path to a safe golden file name.
func sanitizePath(destPath string) string {
	// "secret:foo" → "secret_foo", "/etc/..." → "etc_..."
	s := strings.ReplaceAll(destPath, ":", "_")
	s = strings.TrimPrefix(s, "/")
	return strings.ReplaceAll(s, "/", "_")
}

func TestIntegrationReconcilePipeline(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)

	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	hostname := "node-1" // worker + app-a: exercises multi-feature assignment merging
	resolved, err := r.ResolveHost(t.Context(), hostname)
	require.NoError(t, err)

	// First deploy: all creates
	emptyState := state.NewState()
	cs := reconciler.Diff(resolved.Files, emptyState)
	assert.True(t, cs.HasChanges())
	assert.Equal(t, len(resolved.Files), cs.Summary[reconciler.ActionCreate])
	assert.Equal(t, 0, cs.Summary[reconciler.ActionUpdate])
	assert.Equal(t, 0, cs.Summary[reconciler.ActionDelete])

	// Idempotent: all noops
	cs2 := reconciler.Diff(resolved.Files, stateAfter(cs))
	assert.False(t, cs2.HasChanges())
	assert.Equal(t, len(resolved.Files), cs2.Summary[reconciler.ActionNoop])
}

// stateAfter builds the state a successful apply of cs leaves behind.
func stateAfter(cs *reconciler.Changeset) *state.State {
	st := state.NewState()
	for _, c := range cs.Changes {
		if c.Action != reconciler.ActionDelete {
			st.ManagedFiles[c.DestPath] = state.ManagedFile{Hash: c.NewHash, Category: c.Category}
			if c.ServiceName != "" {
				st.ServiceNames[c.DestPath] = c.ServiceName
			}
		}
	}
	return st
}

const (
	shopPodPath   = "/etc/containers/systemd/picolet/shop.pod"
	shopAPIPath   = "/etc/containers/systemd/picolet/shop-api.container"
	shopProxyPath = "/etc/containers/systemd/picolet/shop-proxy.container"
)

// TestIntegrationReconcilePipelinePod drives the example fleet's pod stack
// (shop.pod.tmpl + two member containers on node-1) through create, update
// and delete: changing the pod and a member restarts only the pod service,
// and removing the stack stops the pod's generated <name>-pod.service.
func TestIntegrationReconcilePipelinePod(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)
	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	resolved, err := r.ResolveHost(t.Context(), "node-1")
	require.NoError(t, err)

	created := reconciler.Diff(resolved.Files, state.NewState())
	pod := findChange(t, created, shopPodPath)
	assert.Equal(t, reconciler.ActionCreate, pod.Action)
	assert.Equal(t, config.CategoryPod, pod.Category)
	assert.Equal(t, "shop-pod.service", pod.ServiceName)
	deployed := stateAfter(created)

	// Update the pod and one member: only the pod service restarts.
	updated := slices.Clone(resolved.Files)
	for i := range updated {
		if updated[i].DestPath == shopPodPath || updated[i].DestPath == shopAPIPath {
			updated[i].Content += "# rev 2\n"
		}
	}
	cs := reconciler.Diff(updated, deployed)
	require.Equal(t, 2, cs.Summary[reconciler.ActionUpdate])
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, "shop-pod.service").Return(nil).Once()
	fw := appliermocks.NewMockFileWriter(t)
	fw.EXPECT().MkdirAll(mock.Anything).Return(nil)
	fw.EXPECT().WriteFile(shopPodPath, mock.Anything).Return(nil).Once()
	fw.EXPECT().WriteFile(shopAPIPath, mock.Anything).Return(nil).Once()
	result, err := applier.New(sys, appliermocks.NewMockPodmanClient(t), fw, false, nil).Apply(t.Context(), cs)
	require.NoError(t, err)
	assert.Equal(t, []string{"shop-pod.service"}, result.RestartedUnits)

	applyDeletes(t, resolved.Files, deployed, map[string]string{
		shopPodPath:   "shop-pod.service",
		shopAPIPath:   "shop-api.service",
		shopProxyPath: "shop-proxy.service",
	})
}

// applyDeletes removes the files keyed in units (DestPath → generated service)
// from the desired files, checks that what remains still validates, and
// applies the resulting deletes: each service, named from state, is stopped
// before its file is removed.
func applyDeletes(t *testing.T, files []resolver.ResolvedFile, deployed *state.State, units map[string]string) {
	t.Helper()
	remaining := slices.DeleteFunc(slices.Clone(files), func(f resolver.ResolvedFile) bool {
		_, ok := units[f.DestPath]
		return ok
	})
	require.NoError(t, validator.ValidateFiles(remaining, validator.Target{}))
	cs := reconciler.Diff(remaining, deployed)
	require.Equal(t, len(units), cs.Summary[reconciler.ActionDelete])
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	fw := appliermocks.NewMockFileWriter(t)
	for path, unit := range units {
		require.Equal(t, unit, findChange(t, cs, path).ServiceName)
		stop := sys.EXPECT().StopUnit(mock.Anything, unit).Return(nil).Once()
		fw.EXPECT().Remove(path).Return(nil).Once().NotBefore(stop)
	}
	_, err := applier.New(sys, appliermocks.NewMockPodmanClient(t), fw, false, nil).Apply(t.Context(), cs)
	require.NoError(t, err)
}

func findChange(t *testing.T, cs *reconciler.Changeset, destPath string) reconciler.Change {
	t.Helper()
	i := slices.IndexFunc(cs.Changes, func(c reconciler.Change) bool { return c.DestPath == destPath })
	require.GreaterOrEqual(t, i, 0, "no change for %s", destPath)
	return cs.Changes[i]
}

const (
	shopAPIBuildPath = "/etc/containers/systemd/picolet/shop-api.build"
	nginxImagePath   = "/etc/containers/systemd/picolet/nginx.image"
)

// TestIntegrationReconcilePipelineBuildAndImage drives node-1's .build and
// .image through create and delete: each is tracked under its generated
// <name>-build.service / <name>-image.service, the build's File= names the
// Containerfile deployed from files/, and removing them together with their
// consumers stops those services before their files are removed.
func TestIntegrationReconcilePipelineBuildAndImage(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)
	r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	resolved, err := r.ResolveHost(t.Context(), "node-1")
	require.NoError(t, err)

	created := reconciler.Diff(resolved.Files, state.NewState())
	build := findChange(t, created, shopAPIBuildPath)
	assert.Equal(t, config.CategoryBuild, build.Category)
	assert.Equal(t, "shop-api-build.service", build.ServiceName)
	image := findChange(t, created, nginxImagePath)
	assert.Equal(t, config.CategoryImage, image.Category)
	assert.Equal(t, "nginx-image.service", image.ServiceName)
	i := slices.IndexFunc(created.Changes, func(c reconciler.Change) bool {
		return c.Category == config.CategoryFile && c.RelPath == "shop-api/Containerfile"
	})
	require.GreaterOrEqual(t, i, 0, "Containerfile not deployed")
	assert.Contains(t, build.NewContent, "File="+created.Changes[i].DestPath+"\n")

	// The consumers go too: a container naming an absent build/image fails validation.
	applyDeletes(t, resolved.Files, stateAfter(created), map[string]string{
		shopAPIBuildPath: "shop-api-build.service",
		nginxImagePath:   "nginx-image.service",
		shopAPIPath:      "shop-api.service",
		shopProxyPath:    "shop-proxy.service",
	})
}

func TestIntegrationMultiHostConsistency(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)

	r, err := resolver.NewKeyless(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	allResolved, err := r.ResolveAll(t.Context())
	require.NoError(t, err)

	// Base resources (network, systemd) should be identical across hosts
	node1 := filesByDest(allResolved["node-1"].Files)
	node2 := filesByDest(allResolved["node-2"].Files)

	// Both should have internal.network with identical content
	n1Net, ok1 := node1["/etc/containers/systemd/picolet/internal.network"]
	n2Net, ok2 := node2["/etc/containers/systemd/picolet/internal.network"]
	require.True(t, ok1, "node-1 should have internal.network")
	require.True(t, ok2, "node-2 should have internal.network")
	assert.Equal(t, n1Net.Content, n2Net.Content)

	// node-1 (worker + app-a) should have nginx.container
	assert.Contains(t, node1, "/etc/containers/systemd/picolet/nginx.container")
	// node-2 (controller) should NOT have nginx.container
	assert.NotContains(t, node2, "/etc/containers/systemd/picolet/nginx.container")

	// node-2 (controller) should have kube and manifest
	assert.Contains(t, node2, "/etc/containers/systemd/picolet/app-stack.kube")
	assert.Contains(t, node2, "/var/lib/picolet/manifests/app/deployment.yml")
	// node-1 (worker) should NOT have these
	assert.NotContains(t, node1, "/etc/containers/systemd/picolet/app-stack.kube")
	assert.NotContains(t, node1, "/var/lib/picolet/manifests/app/deployment.yml")

	// Machine vps-1: each of its three Hosts renders its own Agent plus its
	// siblings, so every Host sees the same set of Agents — one per port, and
	// nothing from another Machine.
	const agentScrapePath = "/var/lib/picolet/files/picolet-agents.yml"
	for _, host := range []string{"vps-1", "vps-1-runner", "vps-1-system"} {
		scrape, ok := filesByDest(allResolved[host].Files)[agentScrapePath]
		require.True(t, ok, "%s should have %s", host, agentScrapePath)
		assert.ElementsMatch(t, []string{"127.0.0.1:9417", "127.0.0.1:9418", "127.0.0.1:9419"},
			loopbackTarget.FindAllString(scrape.Content, -1), host)
	}
	assert.NotContains(t, node1, agentScrapePath)
}

var loopbackTarget = regexp.MustCompile(`127\.0\.0\.1:\d+`)

func filesByDest(files []resolver.ResolvedFile) map[string]resolver.ResolvedFile {
	m := make(map[string]resolver.ResolvedFile, len(files))
	for _, f := range files {
		m[f.DestPath] = f
	}
	return m
}

func TestIntegrationErrorPaths(t *testing.T) {
	t.Parallel()

	t.Run("unknown host", func(t *testing.T) {
		t.Parallel()
		repoFS := os.DirFS(testdataDir)
		cfg, err := config.LoadAll(repoFS)
		require.NoError(t, err)
		r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg})
		require.NoError(t, err)
		_, err = r.ResolveHost(t.Context(), "nonexistent")
		require.Error(t, err)
		var notFound *resolver.HostNotFoundError
		require.ErrorAs(t, err, &notFound)
		assert.Equal(t, "nonexistent", notFound.Hostname)
	})

	pathsCases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "manifests below the first segment",
			files: map[string]string{"app/manifests/deploy.yml": "kind: ConfigMap\n"},
			want:  `app/manifests/deploy.yml: "manifests/" must be the first path segment`,
		},
		{
			name:  "manifests and files in one path",
			files: map[string]string{"app/manifests/files/deploy.yml": "kind: ConfigMap\n"},
			want:  `app/manifests/files/deploy.yml: path has both "manifests/" and "files/"`,
		},
		{
			name:  "unknown extension",
			files: map[string]string{"app/web.contaner": "[Container]\n"},
			want:  `app/web.contaner: unknown extension ".contaner"; move it under files/ or remove it from the listed directory`,
		},
		{
			name:  "extensionless file",
			files: map[string]string{"app/Containerfile": "FROM scratch\n"},
			want:  "app/Containerfile: no file extension; move it under files/ or remove it from the listed directory",
		},
		{
			name:  "destination collision",
			files: map[string]string{"app/web.network": "[Network]\n", "app/nested/web.network": "[Network]\n"},
			want:  "destination collision for /etc/containers/systemd/picolet/web.network: app/nested/web.network, app/web.network",
		},
	}
	for _, tc := range pathsCases {
		t.Run("paths: "+tc.name, func(t *testing.T) {
			t.Parallel()
			require.ErrorContains(t, validatePathsFleet(t, tc.files), tc.want)
		})
	}
}

// Every retired typed list is a load error naming its group and `paths:` as
// the replacement.
func TestIntegrationErrorPathsRetiredTypedLists(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"networks", "systemd", "volumes", "containers", "kube", "pods", "images", "builds", "manifests", "files"} {
		t.Run("typed list "+key, func(t *testing.T) {
			t.Parallel()
			repoFS := fstest.MapFS{
				"fleet.yml":                &fstest.MapFile{Data: []byte("images: {}\nports: {picolet_system_metrics: 9418}\n")},
				"assignments.yml":          &fstest.MapFile{Data: []byte("base: {}\nroles:\n  node:\n    " + key + ":\n      - app/x\nfeatures: {}\n")},
				"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte("hostname: test-host\nrole: node\nfeatures: []\n")},
			}
			_, err := config.LoadAll(repoFS)
			require.EqualError(t, err, "invalid config: assignments.yml: roles.node: '"+key+
				":' was removed; list these files under 'paths:', which derives the category from the file name")
		})
	}
}

// validatePathsFleet runs `picolet validate` on a one-host fleet whose base
// group lists `paths: [app/]`; files maps Fleet paths to content.
func validatePathsFleet(t *testing.T, files map[string]string) error {
	t.Helper()
	repoFS := fstest.MapFS{
		"fleet.yml":                &fstest.MapFile{Data: []byte("images: {}\nports: {picolet_system_metrics: 9418}\n")},
		"assignments.yml":          &fstest.MapFile{Data: []byte("base:\n  paths: [app/]\nroles: {}\nfeatures: {}\n")},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte("hostname: test-host\nexternal_hostname: test-host.ts.net\nrole: node\nfeatures: []\n")},
	}
	for p, content := range files {
		repoFS[p] = &fstest.MapFile{Data: []byte(content)}
	}
	return validateFleet(t, repoFS)
}

func newAggregatedSecretFleetFS(ruleExpr string) fstest.MapFS {
	return fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {picolet_system_metrics: 9418}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`base:
  secrets:
    - secrets/alerts.yml.tmpl
roles: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`hostname: test-host
external_hostname: test-host.ts.net
role: node
features: []
`)},
		"secrets/alerts.yml.tmpl": &fstest.MapFile{Data: []byte(`groups:{{ concatFiles "rules/*.yml" | nindent 2 -}}`)},
		"rules/instance.yml": &fstest.MapFile{Data: []byte(`- name: instance_alerts
  rules:
    - alert: InstanceDown
      expr: ` + ruleExpr + `
      for: 5m
`)},
		"rules/node.yml": &fstest.MapFile{Data: []byte(`- name: node_alerts
  rules:
    - alert: NodeUptimeLow
      expr: node_time_seconds - node_boot_time_seconds < 600
      for: 2m
`)},
	}
}

func TestIntegrationAggregatedSecretFragmentChangeTriggersUpdate(t *testing.T) {
	t.Parallel()

	fsysV1 := newAggregatedSecretFleetFS("up == 0")
	cfgV1, err := config.LoadAll(fsysV1)
	require.NoError(t, err)
	rV1, err := resolver.New(resolver.Config{FS: fsysV1, Config: cfgV1})
	require.NoError(t, err)
	resolvedV1, err := rV1.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)
	require.NoError(t, validator.ValidateFiles(resolvedV1.Files, validator.Target{}))

	initialState := state.NewState()
	csV1 := reconciler.Diff(resolvedV1.Files, initialState)
	require.Equal(t, 1, csV1.Summary[reconciler.ActionCreate])
	for _, c := range csV1.Changes {
		if c.Action == reconciler.ActionDelete {
			continue
		}
		initialState.ManagedFiles[c.DestPath] = state.ManagedFile{Hash: c.NewHash, Category: c.Category}
	}

	fsysV2 := newAggregatedSecretFleetFS("up == 1")
	cfgV2, err := config.LoadAll(fsysV2)
	require.NoError(t, err)
	rV2, err := resolver.New(resolver.Config{FS: fsysV2, Config: cfgV2})
	require.NoError(t, err)
	resolvedV2, err := rV2.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)
	require.NoError(t, validator.ValidateFiles(resolvedV2.Files, validator.Target{}))

	v1Secret := filesByDest(resolvedV1.Files)["secret:alerts"]
	v2Secret := filesByDest(resolvedV2.Files)["secret:alerts"]
	assert.NotEqual(t, v1Secret.Content, v2Secret.Content, "aggregated secret content should change when one fragment changes")

	csV2 := reconciler.Diff(resolvedV2.Files, initialState)
	assert.Equal(t, 1, csV2.Summary[reconciler.ActionUpdate], "changed aggregated secret should produce update")
}

func newSystemdUnitsFleetFS() fstest.MapFS {
	return fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images:\n  node_exporter: \"prom/node-exporter:v1.8\"\n  web: \"nginx:1.27\"\nports: {picolet_system_metrics: 9418}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`base:
  paths:
    - quadlets/internal.network
    - systemd/health.timer
    - quadlets/web.container
    - quadlets/node-exporter.container.tmpl
roles: {}
features: {}
`)},
		"hosts/mon-host/host.yml":   &fstest.MapFile{Data: []byte("hostname: mon-host\nexternal_hostname: mon-host.ts.net\nrole: node\nfeatures: []\n")},
		"quadlets/internal.network": &fstest.MapFile{Data: []byte("[Network]\nInternal=true\n")},
		"quadlets/web.container":    &fstest.MapFile{Data: []byte("[Container]\nImage=nginx:1.27\nContainerName=web\n\n[Install]\nWantedBy=default.target\n")},
		"systemd/health.timer":      &fstest.MapFile{Data: []byte("[Timer]\nOnCalendar=daily\n\n[Install]\nWantedBy=timers.target\n")},
		"quadlets/node-exporter.container.tmpl": &fstest.MapFile{Data: []byte(`[Unit]
Description=Prometheus Node Exporter

[Container]
Image={{ index .Images "node_exporter" }}
ContainerName=node-exporter
Exec=--collector.systemd.unit-include=^({{ range $i, $u := .Host.SystemdUnits }}{{ if $i }}|{{ end }}{{ trimSuffix ".service" $u }}{{ end }})$

[Install]
WantedBy=default.target
`)},
	}
}

// TestIntegrationSystemdUnitsTemplate exercises a node-exporter-style template
// that builds its unit-include regex from .Host.SystemdUnits — including the
// node-exporter unit itself (self-reference).
func TestIntegrationSystemdUnitsTemplate(t *testing.T) {
	t.Parallel()
	fsys := newSystemdUnitsFleetFS()
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := resolver.New(resolver.Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	resolved, err := r.ResolveHost(t.Context(), "mon-host")
	require.NoError(t, err)
	require.NoError(t, validator.ValidateFiles(resolved.Files, validator.Target{}))

	exporter := filesByDest(resolved.Files)["/etc/containers/systemd/picolet/node-exporter.container"]
	require.NotEmpty(t, exporter.Content, "node-exporter.container should be resolved")
	// .Host.SystemdUnits covers the container, network, raw-systemd and the
	// node-exporter unit itself, sorted and deduplicated.
	assert.Contains(t, exporter.Content,
		"unit-include=^(health.timer|internal-network|node-exporter|web)$")

	g := goldie.New(t, goldie.WithFixtureDir("testdata/fixtures"))
	g.Assert(t, "systemd-units/node-exporter.container", []byte(exporter.Content))
}

func TestIntegrationAggregatedSecretMalformedFragmentFailsValidation(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fleet.yml": &fstest.MapFile{Data: []byte("images: {}\nports: {picolet_system_metrics: 9418}\n")},
		"assignments.yml": &fstest.MapFile{Data: []byte(`base:
  secrets:
    - secrets/alerts.yml.tmpl
roles: {}
features: {}
`)},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte(`hostname: test-host
external_hostname: test-host.ts.net
role: node
features: []
`)},
		"secrets/alerts.yml.tmpl": &fstest.MapFile{Data: []byte(`groups:{{ concatFiles "rules/*.yml" | nindent 2 -}}`)},
		"rules/instance.yml":      &fstest.MapFile{Data: []byte("- name: instance_alerts\n  rules:\n    - alert: InstanceDown\n      expr: up == 0\n")},
		"rules/invalid.yml":       &fstest.MapFile{Data: []byte("- name: invalid\n  rules:\n    - alert: Broken\n      expr: [this is broken\n")},
	}
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := resolver.New(resolver.Config{FS: fsys, Config: cfg})
	require.NoError(t, err)
	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)

	err = validator.ValidateFiles(resolved.Files, validator.Target{})
	require.Error(t, err)
	require.ErrorContains(t, err, "secret:alerts")
	require.ErrorContains(t, err, "YAML parse error")
}
