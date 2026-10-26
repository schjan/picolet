package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/sops"
)

// dataTmpl ranges over a map subtree, a list subtree and a nested map, and
// prints a scalar target.
const dataTmpl = `{{- range $k, $v := readSopsData "sops://secrets/db.sops.yaml#/db" }}
{{ $k }}={{ $v }}
{{- end }}
{{- range $i, $v := readSopsData "sops://secrets/db.sops.yaml#/tokens" }}
{{ $i }}: {{ if kindIs "map" $v }}{{ $v.name }}/{{ $v.value }}{{ else }}{{ $v }}{{ end }}
{{- end }}
{{- range $host, $v := readSopsData "sops://secrets/db.sops.yaml#/hosts" }}
{{ $host }} {{ $v.password }}
{{- end }}
port {{ readSopsData "sops://secrets/db.sops.yaml#/db/port" }}`

// The keyless resolve (validate, resolve) renders the real structure with
// every encrypted string leaf as "<sops-secret>" and every encrypted int,
// float or bool leaf as a value of its type; "" and null render empty.
func TestReadSopsDataKeyless(t *testing.T) {
	t.Parallel()
	resolved, err := resolveSopsFleet(t, appTmplBase, map[string]string{appTmpl: dataTmpl}, sopsKeyless, false)
	require.NoError(t, err)
	require.Len(t, resolved.Files, 1)
	p := sops.Placeholder
	assert.Equal(t, `
empty=
nothing=
password=`+p+`
port=0
ratio=0.0
tls=false
user=`+p+`
0: `+p+`
1: `+p+`
2: `+p+`/`+p+`
srv-1.example.net `+p+`
port 0`, resolved.Files[0].Content)
}

// On the Agent readSopsData renders the decrypted values, and a file read by
// both readSopsSecret and readSopsData is decrypted once per resolve.
func TestReadSopsDataDecrypting(t *testing.T) {
	t.Parallel()
	r, probe := sopsResolver(t, appTmplBase, map[string]string{appTmpl: dataTmpl + `
pw {{ readSopsSecret "sops://secrets/db.sops.yaml#/db/password" }}`}, sopsDecrypting, false)
	resolved, err := r.ResolveHost(t.Context(), "test-host")
	require.NoError(t, err)
	require.Len(t, resolved.Files, 1)
	assert.Equal(t, `
empty=
nothing=
password=hunter2-test
port=5432
ratio=0.75
tls=true
user=app
0: alpha-token
1: beta-token
2: gamma/gamma-secret
srv-1.example.net srv1-pass
port 5432
pw hunter2-test`, resolved.Files[0].Content)
	assert.Equal(t, int32(1), probe.opens.Load(), "the SOPS file is read and decrypted once per resolve")
	assert.Equal(t, 1, probe.reader.DecryptedFiles())
}

// A bad pointer or path fails the resolve with the SOPS package's message,
// keyless and on the Agent alike.
func TestReadSopsDataBadRefs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ref  string
		is   error
		want string
	}{
		{name: "bad pointer", ref: "sops://secrets/db.sops.yaml#/db/nope", is: sops.ErrBadPointer, want: `key "nope" not found at "/db"`},
		{name: "dot-dot", ref: "sops://secrets/../secrets/db.sops.yaml#/db", want: "clean relative path"},
		{name: "not a SOPS path", ref: "sops://secrets/db.yaml#/db", is: sops.ErrNotSOPSPath},
	}
	for _, tt := range tests {
		for _, mode := range []sopsReaderMode{sopsKeyless, sopsDecrypting} {
			t.Run(tt.name+"/"+mode.String(), func(t *testing.T) {
				t.Parallel()
				_, err := resolveSopsFleet(t, appTmplBase, map[string]string{
					appTmpl: `{{ range readSopsData "` + tt.ref + `" }}{{ . }}{{ end }}`,
				}, mode, false)
				require.Error(t, err)
				if tt.is != nil {
					require.ErrorIs(t, err, tt.is)
				}
				require.ErrorContains(t, err, tt.want)
				require.ErrorContains(t, err, tt.ref)
				testsops.AssertNoPlaintext(t, err)
			})
		}
	}
}

// The unit-file guard covers readSopsData: a Quadlet, a Raw Unit or a Hook
// definition calling it fails, and the decrypting reader is never asked.
func TestReadSopsDataRejectedInUnitFiles(t *testing.T) {
	t.Parallel()
	call := `{{ range readSopsData "` + guardRef + `" }}{{ . }}{{ end }}`
	tests := []struct {
		name  string
		base  string
		extra map[string]string
		file  string
		kind  string
	}{
		{
			name:  "quadlet",
			base:  "  paths: [units/app.container.tmpl]\n",
			extra: map[string]string{"units/app.container.tmpl": "[Container]\nImage=app\nEnvironment=DB=" + call + "\n"},
			file:  "units/app.container.tmpl",
			kind:  "a Quadlet",
		},
		{
			name:  "raw unit",
			base:  "  paths: [units/backup.service.tmpl]\n",
			extra: map[string]string{"units/backup.service.tmpl": "[Service]\nExecStart=/bin/true\nEnvironment=DB=" + call + "\n"},
			file:  "units/backup.service.tmpl",
			kind:  "a Raw Unit",
		},
		{
			name: "hook definition",
			base: "  services: [app]\n",
			extra: map[string]string{
				"services/app/app.container": "[Container]\nImage=app\nContainerName=app\n",
				"services/app/picolet.yml.tmpl": "hooks:\n  - name: app-reload\n    unit: app.service\n    action: http\n" +
					"    method: GET\n    url: \"http://localhost:1234/-/reload?token=" + call + "\"\n",
			},
			file: "services/app/picolet.yml.tmpl",
			kind: "a Hook definition",
		},
	}
	for _, tt := range tests {
		for _, mode := range []sopsReaderMode{sopsKeyless, sopsRecording} {
			t.Run(tt.name+"/"+mode.String(), func(t *testing.T) {
				t.Parallel()
				r, probe := sopsResolver(t, tt.base, tt.extra, mode, false)
				_, err := r.ResolveHost(t.Context(), "test-host")
				require.ErrorIs(t, err, ErrSopsInUnitFile)
				require.ErrorContains(t, err, tt.file+`: readSopsData "`+refForError(guardRef)+`": `+
					"SOPS values can't be rendered into "+tt.kind)
				testsops.AssertNoPlaintext(t, err)
				assert.False(t, probe.rec.asked(guardRef), "the decrypting reader must not be asked for a guarded ref")
			})
		}
	}
}
