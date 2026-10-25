package picolet_test

import (
	"bufio"
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

	"github.com/schjan/picolet/pkg/sops"
)

const sopsCorpusDir = "testdata/sops"

// sopsCorpus reads a file of the SOPS fixture corpus.
func sopsCorpus(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(sopsCorpusDir, "corpus", name))
	require.NoError(t, err)
	return data
}

// sopsRecipient returns the TEST-ONLY corpus recipient called name.
func sopsRecipient(t *testing.T, name string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(sopsCorpusDir, "recipients.txt"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
	for sc.Scan() {
		if n, key, ok := strings.Cut(sc.Text(), " "); ok && n == name {
			return key
		}
	}
	require.NoError(t, sc.Err())
	t.Fatalf("recipient %q not in corpus", name)
	return ""
}

// sopsHostYML is test-host's host.yml declaring the corpus recipients called
// recipients (none: no age_recipients: key).
func sopsHostYML(t *testing.T, recipients []string) string {
	t.Helper()
	y := "hostname: test-host\nrole: node\n"
	if len(recipients) > 0 {
		y += "age_recipients:\n"
		for _, r := range recipients {
			y += "  - " + sopsRecipient(t, r) + "\n"
		}
	}
	return y
}

// sopsFleet is a valid one-Host Fleet plus files; no Host references them.
func sopsFleet(files map[string]*fstest.MapFile) fstest.MapFS {
	fsys := agentBundleFleet("", "picolet-system", false, "")
	maps.Copy(fsys, files)
	return fsys
}

func data(b []byte) *fstest.MapFile { return &fstest.MapFile{Data: b} }

// validate fails on a *.sops.* file that is not encrypted, whether or not a
// Host references it; the fix is rotation, never a history rewrite.
func TestIntegrationValidateSOPSPlaintextFile(t *testing.T) {
	t.Parallel()
	err := validateFleet(t, sopsFleet(map[string]*fstest.MapFile{
		"secrets/db.sops.yaml":         data(sopsCorpus(t, "plaintext.sops.yaml")),
		"hosts/test-host/app.sops.env": data([]byte("TOKEN=hunter2-test\n")),
		"services/x/no-mac.sops.yaml":  data(sopsCorpus(t, "no-mac.sops.yaml")),
		"secrets/ok.sops.yaml":         data(sopsCorpus(t, "values.sops.yaml")),
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
	assert.NotContains(t, err.Error(), "hunter2")
}

func TestIntegrationValidateSOPSOversizedFile(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte("a"), sops.MaxFileSize+1)
	err := validateFleet(t, sopsFleet(map[string]*fstest.MapFile{"secrets/big.sops.bin": data(big)}))
	require.ErrorIs(t, err, sops.ErrFileTooLarge)
	assert.ErrorContains(t, err, "secrets/big.sops.bin")
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
	srv1, srv2, srv3 := sopsRecipient(t, "srv-1"), sopsRecipient(t, "srv-2"), sopsRecipient(t, "srv-3")
	op := sopsRecipient(t, "operator-1")
	values := sopsCorpus(t, "values.sops.yaml") // srv-1, srv-2, operator-1

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
			want: []string{"`secrets/db.sops.yaml` recipients differ from `.sops.yaml` rule `^secrets/`: +" + op + " −" + srv3 + "; run `sops updatekeys secrets/db.sops.yaml`"},
		},
		{
			name: "first matching rule wins",
			files: map[string]*fstest.MapFile{
				".sops.yaml": sopsRules("  - path_regex: ^secrets/db\\.\n    age: " + srv1 + "\n" +
					"  - path_regex: ^secrets/\n    age: " + op + "," + srv2 + "," + srv1 + "\n"),
				"secrets/db.sops.yaml":  data(values),
				"secrets/app.sops.yaml": data(values),
			},
			want: []string{"`secrets/db.sops.yaml` recipients differ from `.sops.yaml` rule `^secrets/db\\.`: +" + srv2 + " +" + op},
		},
	}
}

func keyGroupRuleCases(t *testing.T) []creationRuleCase {
	t.Helper()
	srv1, srv2 := sopsRecipient(t, "srv-1"), sopsRecipient(t, "srv-2")
	op, pq := sopsRecipient(t, "operator-1"), sopsRecipient(t, "srv-pq")
	values := sopsCorpus(t, "values.sops.yaml") // srv-1, srv-2, operator-1
	keyGroups := sopsCorpus(t, "key-groups.sops.yaml")

	return []creationRuleCase{
		{
			name: "key groups compared group by group",
			files: map[string]*fstest.MapFile{
				".sops.yaml": sopsRules("  - path_regex: \"\"\n    key_groups:\n" +
					"      - age: [" + srv1 + "]\n      - age: [" + srv2 + ", " + op + "]\n      - age: [" + pq + "]\n"),
				"hosts/test-host/app.sops.yaml": data(keyGroups),
			},
			want: []string{"`hosts/test-host/app.sops.yaml` recipients differ from `.sops.yaml` rule 1 (empty path_regex): key group 1: +" + op + "; key group 2: −" + op},
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
			err := validateFleet(t, sopsFleet(tt.files))
			if len(tt.want) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, w := range tt.want {
				assert.ErrorContains(t, err, w)
			}
		})
	}
}

// A symlinked root .sops.yaml on disk is an error, and the link is never
// followed (its target would match and pass).
func TestIntegrationValidateSOPSSymlinkedRulesOnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, f := range sopsFleet(map[string]*fstest.MapFile{
		"rules/sops.yaml":      sopsRules("  - age: " + sopsRecipient(t, "srv-1") + "\n"),
		"secrets/db.sops.yaml": data(sopsCorpus(t, "values.sops.yaml")),
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

// The example Fleet's root .sops.yaml matches its SOPS files
// (TestIntegrationValidate passes); dropping a Recipient from the rule, or
// committing a plaintext SOPS file, fails it.
func TestIntegrationValidateExampleFleetSOPS(t *testing.T) {
	t.Parallel()
	t.Run("rule drift", func(t *testing.T) {
		t.Parallel()
		fsys := exampleFleetMapFS(t)
		op := sopsRecipient(t, "operator-1")
		rules := string(fsys[".sops.yaml"].Data)
		require.Contains(t, rules, ",\n      "+op)
		fsys[".sops.yaml"] = data([]byte(strings.Replace(rules, ",\n      "+op, "", 1)))
		err := validateFleet(t, fsys)
		require.EqualError(t, err, "`secrets/db.sops.yaml` recipients differ from `.sops.yaml` rule `^secrets/`: +"+op+
			"; run `sops updatekeys secrets/db.sops.yaml`")
	})
	t.Run("plaintext file", func(t *testing.T) {
		t.Parallel()
		fsys := exampleFleetMapFS(t)
		fsys["secrets/api.sops.yaml"] = data(sopsCorpus(t, "plaintext.sops.yaml"))
		err := validateFleet(t, fsys)
		require.ErrorContains(t, err, "`secrets/api.sops.yaml` is not SOPS-encrypted (no sops metadata)")
	})
}
