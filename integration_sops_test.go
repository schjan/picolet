package picolet_test

import (
	"bytes"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/sops"
)

// sopsHostYML is test-host's host.yml declaring the corpus recipients called
// recipients (none: no age_recipients: key).
func sopsHostYML(t *testing.T, recipients []string) string {
	t.Helper()
	y := "hostname: test-host\nrole: node\n"
	if len(recipients) > 0 {
		y += "age_recipients:\n"
		for _, r := range recipients {
			y += "  - " + testsops.Recipient(t, r) + "\n"
		}
	}
	return y
}

// sopsFleet is a valid one-Host Fleet plus files; no Host references them,
// and every corpus recipient is an Operator, so the audience pass accepts
// any corpus file.
func sopsFleet(t *testing.T, files map[string]*fstest.MapFile) fstest.MapFS {
	t.Helper()
	fsys := agentBundleFleet("", "picolet-system", false, "")
	fsys[config.OperatorsFile] = corpusOperatorsExcept(t, nil)
	maps.Copy(fsys, files)
	return fsys
}

func data(b []byte) *fstest.MapFile { return &fstest.MapFile{Data: b} }

// validate fails on a *.sops.* file that is not encrypted, whether or not a
// Host references it; the fix is rotation, never a history rewrite.
func TestIntegrationValidateSOPSPlaintextFile(t *testing.T) {
	t.Parallel()
	err := validateFleet(t, sopsFleet(t, map[string]*fstest.MapFile{
		"secrets/db.sops.yaml":         data(testsops.CorpusFile(t, "plaintext.sops.yaml")),
		"hosts/test-host/app.sops.env": data([]byte("TOKEN=hunter2-test\n")),
		"services/x/no-mac.sops.yaml":  data(testsops.CorpusFile(t, "no-mac.sops.yaml")),
		"secrets/ok.sops.yaml":         data(testsops.CorpusFile(t, "values.sops.yaml")),
	}))
	require.Error(t, err)
	for _, want := range []string{
		"`secrets/db.sops.yaml` is not SOPS-encrypted (no sops metadata)",
		"`hosts/test-host/app.sops.env` is not SOPS-encrypted",
		"`services/x/no-mac.sops.yaml` is not SOPS-encrypted (sops.mac missing)",
		"rotate every value it held",
	} {
		require.ErrorContains(t, err, want)
	}
	assert.NotContains(t, err.Error(), "secrets/ok.sops.yaml")
	testsops.AssertNoPlaintext(t, err)
}

func TestIntegrationValidateSOPSOversizedFile(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte("a"), sops.MaxFileSize+1)
	err := validateFleet(t, sopsFleet(t, map[string]*fstest.MapFile{"secrets/big.sops.bin": data(big)}))
	require.ErrorIs(t, err, sops.ErrFileTooLarge)
	require.ErrorContains(t, err, "secrets/big.sops.bin")
	testsops.AssertNoPlaintext(t, err)
}

func sopsRules(body string) *fstest.MapFile {
	return data([]byte("creation_rules:\n" + body))
}

type creationRuleCase struct {
	name  string
	files map[string]*fstest.MapFile
	want  []string // empty: passes
}

func creationRuleCases(t *testing.T) []creationRuleCase {
	t.Helper()
	srv1, srv2, srv3 := testsops.Recipient(t, "srv-1"), testsops.Recipient(t, "srv-2"), testsops.Recipient(t, "srv-3")
	op := testsops.Recipient(t, "operator-1")
	values := testsops.CorpusFile(t, "values.sops.yaml") // srv-1, srv-2, operator-1

	return []creationRuleCase{
		{
			name: "no root .sops.yaml",
			files: map[string]*fstest.MapFile{
				"secrets/db.sops.yaml": data(values),
				"secrets/.sops.yaml":   sopsRules("  - age: " + srv3 + "\n"),
			},
		},
		{
			name: "recipients equal in another order",
			files: map[string]*fstest.MapFile{
				".sops.yaml":           sopsRules("  - path_regex: ^secrets/\n    age: " + op + "," + srv2 + "," + srv1 + "\n"),
				"secrets/db.sops.yaml": data(values),
			},
		},
		{
			name: "no matching rule",
			files: map[string]*fstest.MapFile{
				".sops.yaml":           sopsRules("  - path_regex: ^hosts/\n    age: " + srv3 + "\n"),
				"secrets/db.sops.yaml": data(values),
			},
		},
		{
			name: "recipients differ",
			files: map[string]*fstest.MapFile{
				".sops.yaml":           sopsRules("  - path_regex: ^secrets/\n    age: " + srv1 + "," + srv2 + "," + srv3 + "\n"),
				"secrets/db.sops.yaml": data(values),
			},
			want: []string{"`secrets/db.sops.yaml` recipients differ from the `.sops.yaml` creation rule matching it: +" + op + " −" + srv3 + "; run `sops updatekeys secrets/db.sops.yaml`"},
		},
		{
			name: "first matching rule wins",
			files: map[string]*fstest.MapFile{
				".sops.yaml": sopsRules("  - path_regex: ^secrets/db\\.\n    age: " + srv1 + "\n" +
					"  - path_regex: ^secrets/\n    age: " + op + "," + srv2 + "," + srv1 + "\n"),
				"secrets/db.sops.yaml":  data(values),
				"secrets/app.sops.yaml": data(values),
			},
			want: []string{"`secrets/db.sops.yaml` recipients differ from the `.sops.yaml` creation rule matching it: +" + srv2 + " +" + op},
		},
	}
}

func keyGroupRuleCases(t *testing.T) []creationRuleCase {
	t.Helper()
	srv1, srv2 := testsops.Recipient(t, "srv-1"), testsops.Recipient(t, "srv-2")
	op, pq := testsops.Recipient(t, "operator-1"), testsops.Recipient(t, "srv-pq")
	values := testsops.CorpusFile(t, "values.sops.yaml") // srv-1, srv-2, operator-1
	keyGroups := testsops.CorpusFile(t, "key-groups.sops.yaml")

	return []creationRuleCase{
		{
			name: "key groups compared group by group",
			files: map[string]*fstest.MapFile{
				".sops.yaml": sopsRules("  - path_regex: \"\"\n    key_groups:\n" +
					"      - age: [" + srv1 + "]\n      - age: [" + srv2 + ", " + op + "]\n      - age: [" + pq + "]\n"),
				"hosts/test-host/app.sops.yaml": data(keyGroups),
			},
			want: []string{"`hosts/test-host/app.sops.yaml` recipients differ from the `.sops.yaml` creation rule matching it: key group 1: +" + op + "; key group 2: −" + op},
		},
		{
			name: "key groups equal",
			files: map[string]*fstest.MapFile{
				".sops.yaml": sopsRules("  - key_groups:\n" +
					"      - age: [" + op + ", " + srv1 + "]\n      - age: [" + srv2 + "]\n      - age: [" + pq + "]\n"),
				"hosts/test-host/app.sops.yaml": data(keyGroups),
			},
		},
		{
			name: "invalid rule reported once",
			files: map[string]*fstest.MapFile{
				".sops.yaml":            sopsRules("  - path_regex: \"(\"\n    age: " + srv1 + "\n"),
				"secrets/app.sops.yaml": data(values),
				"secrets/db.sops.yaml":  data(values),
			},
			want: []string{"`.sops.yaml` rule for `secrets/app.sops.yaml` is invalid: can not compile regexp"},
		},
		{
			name: "symlinked .sops.yaml",
			files: map[string]*fstest.MapFile{
				".sops.yaml":           {Data: []byte("rules/sops.yaml"), Mode: fs.ModeSymlink},
				"rules/sops.yaml":      sopsRules("  - age: " + op + "," + srv2 + "," + srv1 + "\n"),
				"secrets/db.sops.yaml": data(values),
			},
			want: []string{".sops.yaml: is a symlink"},
		},
	}
}

func TestIntegrationValidateSOPSCreationRules(t *testing.T) {
	t.Parallel()
	for _, tt := range append(creationRuleCases(t), keyGroupRuleCases(t)...) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateFleet(t, sopsFleet(t, tt.files))
			if len(tt.want) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, w := range tt.want {
				require.ErrorContains(t, err, w)
			}
			assert.Equal(t, len(tt.want), strings.Count(err.Error(), "\n")+1, "one error per want")
			testsops.AssertNoPlaintext(t, err)
		})
	}
}

// A symlinked root .sops.yaml on disk is an error, and the link is never
// followed (its target would match and pass).
func TestIntegrationValidateSOPSSymlinkedRulesOnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, f := range sopsFleet(t, map[string]*fstest.MapFile{
		"rules/sops.yaml":      sopsRules("  - age: " + testsops.Recipient(t, "srv-1") + "\n"),
		"secrets/db.sops.yaml": data(testsops.CorpusFile(t, "values.sops.yaml")),
	}) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, f.Data, 0o600))
	}
	require.NoError(t, os.Symlink("rules/sops.yaml", filepath.Join(dir, ".sops.yaml")))
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })

	err = validateFleet(t, root.FS())
	require.ErrorContains(t, err, ".sops.yaml: is a symlink")
	assert.NotContains(t, err.Error(), "recipients differ")
	testsops.AssertNoPlaintext(t, err)
}

// exampleFleetMapFS copies the example Fleet into a MapFS, so a test can
// change single files.
func exampleFleetMapFS(t *testing.T) fstest.MapFS {
	t.Helper()
	src := os.DirFS(testdataDir)
	out := fstest.MapFS{}
	require.NoError(t, fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		out[path] = data(b)
		return nil
	}))
	return out
}

// The example Fleet's root .sops.yaml has a rule for each of its SOPS files,
// which match them (TestIntegrationValidate passes); dropping a Recipient
// from a rule, or committing a plaintext SOPS file, fails it.
func TestIntegrationValidateExampleFleetSOPS(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ regex, path string }{
		{regex: "^secrets/", path: "secrets/db.sops.yaml"},
		{regex: "^services/web-app/", path: "services/web-app/secrets.sops.yaml"},
	} {
		t.Run("rule drift "+tt.path, func(t *testing.T) {
			t.Parallel()
			fsys := exampleFleetMapFS(t)
			op := testsops.Recipient(t, "operator-1")
			rules := string(fsys[".sops.yaml"].Data)
			i := strings.Index(rules, "path_regex: "+tt.regex+"\n")
			require.GreaterOrEqual(t, i, 0, "no rule %s", tt.regex)
			require.Contains(t, rules[i:], ",\n      "+op)
			fsys[".sops.yaml"] = data([]byte(rules[:i] + strings.Replace(rules[i:], ",\n      "+op, "", 1)))
			err := validateFleet(t, fsys)
			require.EqualError(t, err, "`"+tt.path+"` recipients differ from the `.sops.yaml` creation rule matching it: +"+op+
				"; run `sops updatekeys "+tt.path+"`")
		})
	}
	t.Run("no operators.yml", func(t *testing.T) {
		t.Parallel()
		fsys := exampleFleetMapFS(t)
		delete(fsys, config.OperatorsFile)
		err := validateFleet(t, fsys)
		require.Error(t, err)
		for _, path := range []string{"secrets/db.sops.yaml", "services/web-app/secrets.sops.yaml"} {
			for _, name := range []string{"operator-1", "srv-2"} {
				require.ErrorContains(t, err, "`"+path+"`: age recipient "+testsops.Recipient(t, name)+" (owner: unknown) may not read it")
			}
		}
		testsops.AssertNoPlaintext(t, err)
	})
	t.Run("file no longer referenced", func(t *testing.T) {
		t.Parallel()
		fsys := exampleFleetMapFS(t)
		assignments := string(fsys["assignments.yml"].Data)
		require.Contains(t, assignments, `      - "sops://secrets/db.sops.yaml"`+"\n")
		fsys["assignments.yml"] = data([]byte(strings.Replace(assignments, `      - "sops://secrets/db.sops.yaml"`+"\n", "", 1)))
		delete(fsys, "services/web-app/files/db-pool.conf.tmpl") // its other reference
		err := validateFleet(t, fsys)
		require.EqualError(t, err, "`secrets/db.sops.yaml`: age recipient "+testsops.Recipient(t, "srv-1")+
			" (owner: Host web-1) may not read it: no Host references the file, so only Operators may read it; "+
			"planned retirement: drop it from the `.sops.yaml` rule and run `sops updatekeys secrets/db.sops.yaml`; "+
			"trust lost: drop it, run `sops rotate -i secrets/db.sops.yaml` (new data key) and change every value. "+
			"`updatekeys` revokes nothing")
	})
	t.Run("plaintext file", func(t *testing.T) {
		t.Parallel()
		fsys := exampleFleetMapFS(t)
		fsys["secrets/api.sops.yaml"] = data(testsops.CorpusFile(t, "plaintext.sops.yaml"))
		err := validateFleet(t, fsys)
		require.ErrorContains(t, err, "`secrets/api.sops.yaml` is not SOPS-encrypted (no sops metadata)")
		testsops.AssertNoPlaintext(t, err)
	})
}

// typedSopsManifest is a Deployment whose typed fields come from encrypted
// non-string SOPS leaves: replicas from an int (through readSopsSecret),
// paused from a bool and minReadySeconds from an int (through a
// readSopsData subtree).
const typedSopsManifest = `{{- $db := readSopsData "sops://secrets/db.sops.yaml#/db" -}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  replicas: {{ readSopsSecret "sops://secrets/db.sops.yaml#/db/port" }}
  paused: {{ $db.tls }}
  minReadySeconds: {{ $db.port }}
  selector:
    matchLabels: {app: app}
  template:
    metadata:
      labels: {app: app}
    spec:
      containers:
        - name: app
          image: docker.io/library/nginx:latest
`

// A typed field of a strictly validated manifest filled from an encrypted
// int or bool leaf passes validate, as the Agent's decrypted 5432/true
// does: keyless, such a leaf renders a value of its type, not the string
// placeholder.
func TestIntegrationValidateTypedSopsLeafInManifest(t *testing.T) {
	t.Parallel()
	fsys := sopsFleet(t, map[string]*fstest.MapFile{
		"assignments.yml":          data([]byte("base:\n  paths: [manifests/app.yaml.tmpl]\nroles: {}\nfeatures: {}\n")),
		"manifests/app.yaml.tmpl":  data([]byte(typedSopsManifest)),
		"hosts/test-host/host.yml": data([]byte(sopsHostYML(t, []string{"srv-1"}))),
		"secrets/db.sops.yaml":     data(testsops.CorpusFile(t, "values.sops.yaml")),
		config.OperatorsFile:       corpusOperatorsExcept(t, []string{"srv-1"}),
	})
	require.NoError(t, validateFleet(t, fsys))
}
