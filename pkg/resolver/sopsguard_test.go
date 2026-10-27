package resolver

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/config"
)

const guardRef = "sops://secrets/db.sops.yaml#/db/password"

// recordingSopsReader stands in for the Agent's decrypting reader: it
// returns "decrypted" for every ref and records which refs it was asked for.
type recordingSopsReader struct {
	mu   sync.Mutex
	refs []string
}

func (r *recordingSopsReader) ReadAll(_ context.Context, refs []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		r.refs = append(r.refs, ref)
		out[ref] = "decrypted"
	}
	return out, nil
}

func (r *recordingSopsReader) ReadData(_ context.Context, ref string) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
	return "decrypted", nil
}

func (r *recordingSopsReader) asked(ref string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.refs, ref)
}

// A SOPS value rendered into a Quadlet, a Raw Unit or a Hook definition is
// readable beyond the Host's user: the render fails, naming the file and the
// ref and pointing to Secret=, in both the keyless and the decrypting path,
// and the decrypting reader is never asked for the ref.
func TestReadSopsSecretRejectedInUnitFiles(t *testing.T) {
	t.Parallel()
	call := `{{ readSopsSecret "` + guardRef + `" }}`
	tests := []struct {
		name, base string
		extra      map[string]string
		file, kind string
	}{
		{
			name:  "quadlet",
			base:  "  paths: [units/app.container.tmpl]\n",
			extra: map[string]string{"units/app.container.tmpl": "[Container]\nImage=app\nEnvironment=DB_PASSWORD=" + call + "\n"},
			file:  "units/app.container.tmpl",
			kind:  "a Quadlet",
		},
		{
			name: "quadlet through renderTemplate",
			base: "  paths: [units/app.container.tmpl]\n",
			extra: map[string]string{
				"units/app.container.tmpl": "[Container]\nImage=app\n{{ renderTemplate \"partials/env.tmpl\" . }}",
				"partials/env.tmpl":        "Environment=DB_PASSWORD=" + call + "\n",
			},
			file: "units/app.container.tmpl",
			kind: "a Quadlet",
		},
		{
			name:  "raw unit",
			base:  "  paths: [units/backup.service.tmpl]\n",
			extra: map[string]string{"units/backup.service.tmpl": "[Service]\nExecStart=/bin/true\nEnvironment=DB_PASSWORD=" + call + "\n"},
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
				resolved, err := r.ResolveHost(t.Context(), "test-host")
				require.Error(t, err)
				assert.Nil(t, resolved)
				require.ErrorIs(t, err, ErrSopsInUnitFile)
				require.ErrorContains(t, err, tt.file+`: readSopsSecret "`+refForError(guardRef)+`": `+
					"SOPS values can't be rendered into "+tt.kind)
				require.ErrorContains(t, err, "use Secret= (or a secret mounted as a file) instead")
				assert.False(t, probe.rec.asked(guardRef), "the decrypting reader must not be asked for a guarded ref")
			})
		}
	}
}

// readSopsSecret still renders into a manifest, a data file and a
// secrets/*.tmpl, also through a partial a guarded Quadlet does not call.
func TestReadSopsSecretRendersOutsideUnitFiles(t *testing.T) {
	t.Parallel()
	call := `{{ readSopsSecret "` + guardRef + `" }}`
	extra := map[string]string{ //nolint:gosec // test templates, no credentials
		"manifests/app/deploy.yml.tmpl": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\nstringData:\n  value: " + call + "\n",
		"files/app/app.conf.tmpl":       "value=" + call + "\n",
		"secrets/app.yml.tmpl":          `{{ renderTemplate "partials/value.tmpl" . }}`,
		"partials/value.tmpl":           "value: " + call,
		"units/app.container.tmpl":      "[Container]\nImage=app\n",
	}
	base := "  paths: [manifests/, files/, units/]\n  secrets: [secrets/app.yml.tmpl]\n"
	for _, mode := range []sopsReaderMode{sopsKeyless, sopsRecording} {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()
			resolved, err := resolveSopsFleet(t, base, extra, mode, false)
			require.NoError(t, err)
			value := "decrypted"
			if mode == sopsKeyless {
				value = "<sops-secret>"
			}
			contents := make(map[string]string, len(resolved.Files))
			for _, f := range resolved.Files {
				contents[f.SrcPath] = f.Content
			}
			assert.Contains(t, contents["manifests/app/deploy.yml.tmpl"], "value: "+value)
			assert.Equal(t, "value="+value+"\n", contents["files/app/app.conf.tmpl"])
			assert.Equal(t, "value: "+value, contents["secrets/app.yml.tmpl"])
		})
	}
}

// The guard runs before the missing-reader placeholder and before strict
// mode: a guarded file fails with ErrSopsInUnitFile when the Fleet has no
// SOPS reader and when the resolver is strict (bootstrap).
func TestReadSopsSecretGuardPrecedesStrictAndMissingReader(t *testing.T) {
	t.Parallel()
	extra := map[string]string{
		"units/app.container.tmpl": "[Container]\nImage=app\nEnvironment=DB_PASSWORD={{ readSopsSecret \"" + guardRef + "\" }}\n",
	}
	base := "  paths: [units/app.container.tmpl]\n"
	tests := []struct {
		name   string
		strict bool
		mode   sopsReaderMode
	}{
		{name: "no sops reader", mode: sopsNoReader},
		{name: "strict", strict: true, mode: sopsKeyless},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveSopsFleet(t, base, extra, tt.mode, tt.strict)
			require.ErrorIs(t, err, ErrSopsInUnitFile)
			testsops.AssertNoPlaintext(t, err)
		})
	}
}

// A guarded file may build its sops:// pointer from a host secret's value
// (readSecretFile is not a provider and not guarded); only SOPS values are
// scrubbed, so the guard's error names the ref without its pointer
// (refForError) and never shows the host secret's value.
func TestSopsGuardErrorKeepsDynamicRefOut(t *testing.T) {
	t.Parallel()
	mapFS := pathsFleetFS("  paths: [units/app.container.tmpl]\n", map[string]string{
		"secrets/db.sops.yaml":     testsops.CorpusText(t, "values.sops.yaml"),
		"hosts/test-host/host.yml": sopsHostYAML(t, "srv-1"),
		"units/app.container.tmpl": "[Container]\nImage=app\nEnvironment=DB_PASSWORD=" +
			`{{ readSopsSecret (printf "sops://secrets/db.sops.yaml#/%s" (readSecretFile "user")) }}` + "\n",
	})
	cfg, err := config.LoadAll(mapFS)
	require.NoError(t, err)
	secrets := func(string) (string, error) { return secretItem, nil }
	r, err := New(Config{FS: mapFS, Config: cfg, SecretReader: secrets, Sops: &recordingSopsReader{}})
	require.NoError(t, err)

	_, err = r.ResolveHost(t.Context(), "test-host")
	require.ErrorIs(t, err, ErrSopsInUnitFile)
	require.ErrorContains(t, err, `readSopsSecret "sops://secrets/db.sops.yaml#…"`)
	assert.NotContains(t, err.Error(), secretItem)
}
