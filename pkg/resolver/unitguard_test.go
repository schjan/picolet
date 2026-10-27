package resolver

import (
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

// resolveGuardFleet resolves test-host of a paths Fleet with rc's readers and
// strict mode.
func resolveGuardFleet(t *testing.T, rc Config, base string, extra map[string]string) (*ResolvedHost, error) {
	t.Helper()
	fsys := pathsFleetFS(base, extra)
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	rc.FS, rc.Config = fsys, cfg
	r, err := New(rc)
	require.NoError(t, err)
	return r.ResolveHost(t.Context(), "test-host")
}

// unitFileCase is one guarded render: a Fleet whose base group (base) and
// files (extra) render a call into file, a unit file of kind.
type unitFileCase struct {
	name, base string
	extra      map[string]string
	file, kind string
}

// unitFileCases are the guarded renders of call: a Quadlet (directly and
// through renderTemplate), a Raw Unit and a Hook definition.
func unitFileCases(call string) []unitFileCase {
	return []unitFileCase{
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
					rec := &recordingSopsReader{}
					var reader SecretRefReader
					if withReader {
						reader = rec.ReadAll
					}
					resolved, err := resolveGuardFleet(t, s.config(reader), tt.base, tt.extra)
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
			resolved, err := resolveGuardFleet(t, s.config((&recordingSopsReader{}).ReadAll), base, extra)
			require.NoError(t, err)
			contents := make(map[string]string, len(resolved.Files))
			for _, f := range resolved.Files {
				contents[f.SrcPath] = f.Content
			}
			assert.Contains(t, contents["manifests/app/deploy.yml.tmpl"], "value: decrypted")
			assert.Equal(t, "value=decrypted\n", contents["files/app/app.conf.tmpl"])
			assert.Equal(t, "value: decrypted", contents["secrets/app.yml.tmpl"])
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
			rc := s.config(nil)
			rc.Strict = true
			_, err := resolveGuardFleet(t, rc, "  paths: [units/app.container.tmpl]\n", extra)
			require.ErrorIs(t, err, s.err)
		})
	}
}

// A template that passes a value instead of a ref (here a host secret's) gets
// the guard's error without that value: refForError names only a provider
// ref's scheme and vault.
func TestUnitFileGuardErrorKeepsNonRefArgumentOut(t *testing.T) {
	t.Parallel()
	const value = "https://user:hunter2@host/x" //nolint:gosec // G101: a fake secret value the error must not show
	for _, s := range unitGuardSchemes {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			extra := map[string]string{
				"units/app.container.tmpl": "[Container]\nImage=app\nEnvironment=TOKEN={{ " + s.fn + ` (readSecretFile "tok") }}` + "\n",
			}
			rc := s.config(nil)
			rc.SecretReader = func(string) (string, error) { return value, nil }
			_, err := resolveGuardFleet(t, rc, "  paths: [units/app.container.tmpl]\n", extra)
			require.ErrorIs(t, err, s.err)
			require.ErrorContains(t, err, s.fn+` "…"`)
			assert.NotContains(t, err.Error(), "hunter2")
		})
	}
}
