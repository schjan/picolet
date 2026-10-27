package resolver

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
)

// unitGuardScheme is one provider whose values the unit-file guard keeps out
// of Quadlets, Raw Units and Hook definitions, besides SOPS
// (sopsguard_test.go).
type unitGuardScheme struct {
	name, fn, ref, word string
	err                 error
	// config returns a resolver Config with reader as this scheme's reader.
	config func(reader SecretRefReader) Config
}

var unitGuardSchemes = []unitGuardScheme{
	{
		name: "op", fn: "readOpSecret", ref: "op://vault/app/token", word: "1Password", err: ErrOpInUnitFile,
		config: func(reader SecretRefReader) Config { return Config{OpSecretReader: reader} },
	},
	{
		name: "pass", fn: "readProtonPassSecret", ref: "pass://share/app/token", word: "Proton Pass", err: ErrProtonPassInUnitFile,
		config: func(reader SecretRefReader) Config { return Config{PPSecretReader: reader} },
	},
}

// recordingRefReader resolves every ref to "resolved" and records which refs
// it was asked for.
type recordingRefReader struct {
	mu   sync.Mutex
	refs []string
}

func (r *recordingRefReader) read(_ context.Context, refs []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		r.refs = append(r.refs, ref)
		out[ref] = "resolved"
	}
	return out, nil
}

func (r *recordingRefReader) asked(ref string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.refs, ref)
}

// resolveGuardFleet resolves test-host of a paths Fleet with s's reader
// (nil: none) and strict mode as given.
func resolveGuardFleet(t *testing.T, s unitGuardScheme, base string, extra map[string]string, reader SecretRefReader, strict bool) (*ResolvedHost, error) {
	t.Helper()
	fsys := pathsFleetFS(base, extra)
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	rc := s.config(reader)
	rc.FS, rc.Config, rc.Strict = fsys, cfg, strict
	r, err := New(rc)
	require.NoError(t, err)
	return r.ResolveHost(t.Context(), "test-host")
}

// unitFileCases are the guarded renders of call: a Quadlet (directly and
// through renderTemplate), a Raw Unit and a Hook definition.
func unitFileCases(call string) []struct {
	name, base string
	extra      map[string]string
	file, kind string
} {
	return []struct {
		name, base string
		extra      map[string]string
		file, kind string
	}{
		{
			name:  "quadlet",
			base:  "  paths: [units/app.container.tmpl]\n",
			extra: map[string]string{"units/app.container.tmpl": "[Container]\nImage=app\nEnvironment=TOKEN=" + call + "\n"},
			file:  "units/app.container.tmpl",
			kind:  "a Quadlet",
		},
		{
			name: "quadlet through renderTemplate",
			base: "  paths: [units/app.container.tmpl]\n",
			extra: map[string]string{
				"units/app.container.tmpl": "[Container]\nImage=app\n{{ renderTemplate \"partials/env.tmpl\" . }}",
				"partials/env.tmpl":        "Environment=TOKEN=" + call + "\n",
			},
			file: "units/app.container.tmpl",
			kind: "a Quadlet",
		},
		{
			name:  "raw unit",
			base:  "  paths: [units/backup.service.tmpl]\n",
			extra: map[string]string{"units/backup.service.tmpl": "[Service]\nExecStart=/bin/true\nEnvironment=TOKEN=" + call + "\n"},
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
}

// An op:// or pass:// value rendered into a Quadlet, a Raw Unit or a Hook
// definition is readable beyond the Host's user, as a SOPS value is: the
// render fails, naming the file and the ref and pointing to Secret=, with
// and without the provider's reader (validate and resolve have none), and
// the reader is never asked for the ref.
func TestProviderSecretRejectedInUnitFiles(t *testing.T) {
	t.Parallel()
	for _, s := range unitGuardSchemes {
		for _, tt := range unitFileCases(`{{ ` + s.fn + ` "` + s.ref + `" }}`) {
			for _, withReader := range []bool{false, true} {
				name := s.name + "/" + tt.name + "/no reader"
				if withReader {
					name = s.name + "/" + tt.name + "/reader"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					rec := &recordingRefReader{}
					var reader SecretRefReader
					if withReader {
						reader = rec.read
					}
					resolved, err := resolveGuardFleet(t, s, tt.base, tt.extra, reader, false)
					assert.Nil(t, resolved)
					require.ErrorIs(t, err, s.err)
					require.ErrorContains(t, err, tt.file+`: `+s.fn+` "`+refForError(s.ref)+`": `+
						s.word+" values can't be rendered into "+tt.kind)
					require.ErrorContains(t, err, "use Secret= (or a secret mounted as a file) instead")
					assert.False(t, rec.asked(s.ref), "the reader must not be asked for a guarded ref")
				})
			}
		}
	}
}

// readOpSecret and readProtonPassSecret still render into a manifest, a data
// file and a secrets/*.tmpl, also through a partial.
func TestProviderSecretRendersOutsideUnitFiles(t *testing.T) {
	t.Parallel()
	for _, s := range unitGuardSchemes {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			call := `{{ ` + s.fn + ` "` + s.ref + `" }}`
			extra := map[string]string{ //nolint:gosec // test templates, no credentials
				"manifests/app/deploy.yml.tmpl": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\nstringData:\n  value: " + call + "\n",
				"files/app/app.conf.tmpl":       "value=" + call + "\n",
				"secrets/app.yml.tmpl":          `{{ renderTemplate "partials/value.tmpl" . }}`,
				"partials/value.tmpl":           "value: " + call,
				"units/app.container.tmpl":      "[Container]\nImage=app\n",
			}
			base := "  paths: [manifests/, files/, units/]\n  secrets: [secrets/app.yml.tmpl]\n"
			rec := &recordingRefReader{}
			resolved, err := resolveGuardFleet(t, s, base, extra, rec.read, false)
			require.NoError(t, err)
			contents := make(map[string]string, len(resolved.Files))
			for _, f := range resolved.Files {
				contents[f.SrcPath] = f.Content
			}
			assert.Contains(t, contents["manifests/app/deploy.yml.tmpl"], "value: resolved")
			assert.Equal(t, "value=resolved\n", contents["files/app/app.conf.tmpl"])
			assert.Equal(t, "value: resolved", contents["secrets/app.yml.tmpl"])
		})
	}
}

// The guard runs before strict mode (bootstrap, validate's Agent-bundle
// pass): a guarded file fails with the unit-file error there too.
func TestProviderSecretGuardPrecedesStrict(t *testing.T) {
	t.Parallel()
	for _, s := range unitGuardSchemes {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			extra := map[string]string{
				"units/app.container.tmpl": "[Container]\nImage=app\nEnvironment=TOKEN={{ " + s.fn + ` "` + s.ref + "\" }}\n",
			}
			_, err := resolveGuardFleet(t, s, "  paths: [units/app.container.tmpl]\n", extra, nil, true)
			require.ErrorIs(t, err, s.err)
		})
	}
}
