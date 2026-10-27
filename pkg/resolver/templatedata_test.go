package resolver

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
)

// topologyTemplate renders every topology field the template data exposes.
const topologyTemplate = `self {{ .Host.Hostname }} machine={{ .Host.Machine }} user={{ .Host.User }} rootful={{ .Host.Rootful }} port={{ .Host.ListenPort }}
{{ range siblings }}sibling {{ .Hostname }} machine={{ .Machine }} user={{ .User }} rootful={{ .Rootful }} port={{ .ListenPort }}
{{ end }}{{ range .Fleet.Hosts }}fleet {{ .Hostname }} machine={{ .Machine }} user={{ .User }} rootful={{ .Rootful }} port={{ .ListenPort }}
{{ end }}`

// topologyFleetFS is one Machine "vps" with four Hosts plus a single-Host
// Machine "other". Directory "a-ci" holds Host "vps-ci", so directory order
// and hostname order disagree; it spells its Machine "VPS", which is the same
// Machine and is rendered as written.
func topologyFleetFS() fstest.MapFS {
	host := func(body string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("role: node\n" + body)}
	}
	return fstest.MapFS{
		"fleet.yml":                 &fstest.MapFile{Data: []byte("images: {}\nports:\n  picolet_metrics: 9417\n  picolet_system_metrics: 9418\n")},
		"assignments.yml":           &fstest.MapFile{Data: []byte("base:\n  paths:\n    - files/topology.txt.tmpl\nroles: {}\nfeatures: {}\n")},
		"files/topology.txt.tmpl":   &fstest.MapFile{Data: []byte(topologyTemplate)},
		"hosts/vps/host.yml":        host("hostname: vps\nuser: pi\n"),
		"hosts/vps-system/host.yml": host("hostname: vps-system\nmachine: vps\n"),
		"hosts/vps-runner/host.yml": host("hostname: vps-runner\nmachine: vps\nuser: runner\nlisten_port: 9419\n"),
		"hosts/a-ci/host.yml":       host("hostname: vps-ci\nmachine: VPS\nuser: ci\nlisten_port: 9420\n"),
		"hosts/other/host.yml":      host("hostname: other\n"),
	}
}

func resolveTopology(t *testing.T, hostname string) string {
	t.Helper()
	fsys := topologyFleetFS()
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)
	resolved, err := r.ResolveHost(t.Context(), hostname)
	require.NoError(t, err)
	require.Len(t, resolved.Files, 1)
	return resolved.Files[0].Content
}

func TestTemplateDataTopology(t *testing.T) {
	t.Parallel()
	const fleetHosts = `fleet vps-ci machine=VPS user=ci rootful=false port=9420
fleet other machine=other user= rootful=true port=9418
fleet vps machine=vps user=pi rootful=false port=9417
fleet vps-runner machine=vps user=runner rootful=false port=9419
fleet vps-system machine=vps user= rootful=true port=9418
`
	tests := []struct {
		name     string
		hostname string
		want     string
	}{
		{
			name:     "siblings are the other Hosts on the Machine, sorted by hostname",
			hostname: "vps-runner",
			want: `self vps-runner machine=vps user=runner rootful=false port=9419
sibling vps machine=vps user=pi rootful=false port=9417
sibling vps-ci machine=VPS user=ci rootful=false port=9420
sibling vps-system machine=vps user= rootful=true port=9418
` + fleetHosts,
		},
		{
			name:     "rootful Host",
			hostname: "vps-system",
			want: `self vps-system machine=vps user= rootful=true port=9418
sibling vps machine=vps user=pi rootful=false port=9417
sibling vps-ci machine=VPS user=ci rootful=false port=9420
sibling vps-runner machine=vps user=runner rootful=false port=9419
` + fleetHosts,
		},
		{
			name:     "single-Host Machine has no siblings",
			hostname: "other",
			want:     "self other machine=other user= rootful=true port=9418\n" + fleetHosts,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, resolveTopology(t, tt.hostname))
		})
	}
}

// .Fleet.Config is a value view of fleet.yml: Images, Ports and Bootstrap
// render, and no template reaches the shared config's yaml.Node, whose
// pointer methods (Encode, SetString) would let one render hand a value to
// a later one (an op:// value into a Hook definition, past the unit-file
// guard) or to another Host's resolve.
func TestFleetConfigTemplateData(t *testing.T) {
	t.Parallel()
	t.Run("value fields render", func(t *testing.T) {
		t.Parallel()
		fsys := pathsFleetFS("  paths: [files/fleet.txt.tmpl]\n", map[string]string{
			"files/fleet.txt.tmpl": `{{ .Fleet.Config.Ports.picolet_system_metrics }} {{ len .Fleet.Config.Images }} {{ len .Fleet.Config.Bootstrap.Files }}`,
		})
		resolved, err := resolveFS(t, fsys, Config{})
		require.NoError(t, err)
		require.Len(t, resolved.Files, 1)
		assert.Equal(t, "9418 0 0", resolved.Files[0].Content)
	})
	t.Run("no shared node", func(t *testing.T) {
		t.Parallel()
		fsys := pathsFleetFS("  paths: [files/app.conf.tmpl]\n  services: [app]\n", map[string]string{
			"files/app.conf.tmpl":        `{{ .Fleet.Config.RetiredPrometheus.Encode (readOpSecret "op://vault/item/user") }}`,
			"services/app/app.container": "[Container]\nImage=app\nContainerName=app\n",
			"services/app/picolet.yml.tmpl": "hooks:\n  - name: app-reload\n    secrets: [app_config]\n    unit: app.service\n    action: http\n" +
				"    method: GET\n    url: \"http://localhost:1234/?token={{ .Fleet.Config.RetiredPrometheus.Value }}\"\n",
		})
		resolved, err := resolveFS(t, fsys, Config{OpSecretReader: userReader})
		require.ErrorContains(t, err, "can't evaluate field RetiredPrometheus")
		assert.Nil(t, resolved)
	})
}

// resolveFS resolves test-host of fsys with rc's readers.
func resolveFS(t *testing.T, fsys fstest.MapFS, rc Config) (*ResolvedHost, error) {
	t.Helper()
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	rc.FS, rc.Config = fsys, cfg
	r, err := New(rc)
	require.NoError(t, err)
	return r.ResolveHost(t.Context(), "test-host")
}
