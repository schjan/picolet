package sops_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/sops"
)

// TestCreationRulesMatchSOPSCLI checks the rule matcher against the rule the
// sops CLI chose for each file when the corpus was generated
// (testdata/sops/creation-rules/expected.json, `task sops:corpus`).
func TestCreationRulesMatchSOPSCLI(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(corpusDir, "creation-rules", "expected.json"))
	require.NoError(t, err)
	var golden struct {
		Cases []struct {
			Name     string         `json:"name"`
			Rules    []string       `json:"rules"`
			Expected map[string]int `json:"expected"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(data, &golden))
	require.NotEmpty(t, golden.Cases)

	rcp := corpusRecipient(t, "srv-1")
	for _, c := range golden.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			var cfg strings.Builder
			cfg.WriteString("creation_rules:\n")
			for _, re := range c.Rules {
				quoted, err := json.Marshal(re)
				require.NoError(t, err)
				fmt.Fprintf(&cfg, "  - path_regex: %s\n    age: %s\n", quoted, rcp)
			}
			rules, err := sops.ParseCreationRules([]byte(cfg.String()))
			require.NoError(t, err)
			for file, want := range c.Expected {
				rule, ok, err := rules.Match(file)
				require.NoError(t, err, file)
				if want == -1 {
					assert.False(t, ok, "%s: sops found no matching rule", file)
					continue
				}
				if assert.True(t, ok, "%s: sops chose rule %d", file, want) {
					assert.Equal(t, want, rule.Index, file)
					assert.Equal(t, c.Rules[want], rule.PathRegex, file)
				}
			}
		})
	}
}

func TestCreationRulesMatchBadRegex(t *testing.T) {
	t.Parallel()
	rules, err := sops.ParseCreationRules([]byte("creation_rules:\n  - path_regex: ^ok/\n  - path_regex: \"(\"\n"))
	require.NoError(t, err)
	// As upstream: a rule after the match is never compiled.
	_, ok, err := rules.Match("ok/db.sops.yaml")
	require.NoError(t, err)
	assert.True(t, ok)
	_, _, err = rules.Match("other/db.sops.yaml")
	require.ErrorContains(t, err, `creation rule 2: path_regex "("`)
}

// inspectSOPS inspects a corpus file that must be a SOPS file.
func inspectSOPS(t *testing.T, name string) *sops.File {
	t.Helper()
	f := inspectCorpus(t, name)
	require.True(t, f.IsSOPS, f.NotSOPSReason)
	return f
}

func matchOnly(t *testing.T, cfg string) *sops.CreationRule {
	t.Helper()
	rules, err := sops.ParseCreationRules([]byte(cfg))
	require.NoError(t, err)
	rule, ok, err := rules.Match("secrets/db.sops.yaml")
	require.NoError(t, err)
	require.True(t, ok)
	return rule
}

type checkRecipientsCase struct {
	name, file, cfg string
	// want is the error's text after the path; empty passes.
	want string
}

func checkRecipientsCases(t *testing.T) []checkRecipientsCase {
	t.Helper()
	srv1, srv2, srv3 := corpusRecipient(t, "srv-1"), corpusRecipient(t, "srv-2"), corpusRecipient(t, "srv-3")
	op, pq := corpusRecipient(t, "operator-1"), corpusRecipient(t, "srv-pq")
	const update = "run `sops updatekeys secrets/db.sops.yaml`"

	return []checkRecipientsCase{
		{
			name: "equal, other order, CSV string",
			file: "values.sops.yaml",
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age: " + op + ", " + srv2 + "," + srv1 + "\n",
		},
		{
			name: "equal, YAML list, upper case label",
			file: "values.sops.yaml",
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age:\n      - " + strings.ToUpper(srv1) + "\n      - " + srv2 + "\n      - " + op + "\n",
		},
		{
			name: "file has extra and lacks one",
			file: "values.sops.yaml",
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age: " + srv1 + "," + srv2 + "," + srv3 + "\n",
			want: "recipients differ from `.sops.yaml` rule `^secrets/`: +" + op + " −" + srv3 + "; " + update,
		},
		{
			name: "empty path_regex",
			file: "values.sops.json",
			cfg:  "creation_rules:\n  - age: " + srv1 + "\n",
			want: "recipients differ from `.sops.yaml` rule 1 (empty path_regex): +" + srv2 + "; " + update,
		},
		{
			name: "key groups, set per group",
			file: "key-groups.sops.yaml",
			cfg: "creation_rules:\n  - path_regex: ^secrets/\n    shamir_threshold: 1\n    key_groups:\n" +
				"      - age: [" + op + ", " + srv1 + "]\n      - age: [" + srv2 + "]\n      - age: [" + pq + "]\n",
		},
		{
			name: "key groups, merge",
			file: "key-groups.sops.yaml",
			cfg: "creation_rules:\n  - path_regex: ^secrets/\n    key_groups:\n" +
				"      - merge:\n          - age: [" + op + "]\n        age: [" + srv1 + "]\n      - age: [" + srv2 + "]\n      - age: [" + pq + "]\n",
		},
		{
			name: "key groups, same recipients in other groups",
			file: "key-groups.sops.yaml",
			cfg: "creation_rules:\n  - path_regex: ^secrets/\n    key_groups:\n" +
				"      - age: [" + srv1 + "]\n      - age: [" + srv2 + ", " + op + "]\n      - age: [" + pq + "]\n",
			want: "recipients differ from `.sops.yaml` rule `^secrets/`: key group 1: +" + op + "; key group 2: −" + op + "; " + update,
		},
		{
			name: "key groups vs single group rule",
			file: "key-groups.sops.yaml",
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age: " + srv1 + "," + op + "\n",
			want: "recipients differ from `.sops.yaml` rule `^secrets/`: file has 3 key groups, rule has 1; key group 2: +" + srv2 + "; key group 3: +" + pq + "; " + update,
		},
		{
			name: "non-age keys ignored",
			file: "non-age.sops.yaml",
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age: " + srv1 + "," + srv2 + "\n    pgp: 0000000000000000000000000000000000000000\n",
		},
	}
}

func TestCheckRecipients(t *testing.T) {
	t.Parallel()
	for _, tt := range checkRecipientsCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := matchOnly(t, tt.cfg).CheckRecipients("secrets/db.sops.yaml", inspectSOPS(t, tt.file))
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, "`secrets/db.sops.yaml` "+tt.want)
		})
	}
}

func TestCheckRecipientsBadRuleRecipient(t *testing.T) {
	t.Parallel()
	rule := matchOnly(t, "creation_rules:\n  - path_regex: ^secrets/\n    age: [1, 2]\n")
	err := rule.CheckRecipients("secrets/db.sops.yaml", inspectSOPS(t, "values.sops.yaml"))
	require.ErrorContains(t, err, "`.sops.yaml` rule `^secrets/`: age")
}

func TestReadCreationRules(t *testing.T) {
	t.Parallel()
	body := []byte("creation_rules:\n  - path_regex: ^secrets/\n")
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		rules, err := sops.ReadCreationRules(fstest.MapFS{})
		require.NoError(t, err)
		_, ok, err := rules.Match("secrets/db.sops.yaml")
		require.NoError(t, err)
		assert.False(t, ok)
	})
	t.Run("regular file", func(t *testing.T) {
		t.Parallel()
		rules, err := sops.ReadCreationRules(fstest.MapFS{".sops.yaml": {Data: body}})
		require.NoError(t, err)
		_, ok, err := rules.Match("secrets/db.sops.yaml")
		require.NoError(t, err)
		assert.True(t, ok)
	})
	t.Run("symlink is never followed", func(t *testing.T) {
		t.Parallel()
		fsys := fstest.MapFS{
			".sops.yaml":     {Data: []byte("elsewhere.yaml"), Mode: fs.ModeSymlink},
			"elsewhere.yaml": {Data: body},
		}
		_, err := sops.ReadCreationRules(fsys)
		require.ErrorContains(t, err, ".sops.yaml: is a symlink")
	})
	t.Run("symlink on disk", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "elsewhere.yaml"), body, 0o600))
		require.NoError(t, os.Symlink("elsewhere.yaml", filepath.Join(dir, ".sops.yaml")))
		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = root.Close() })
		_, err = sops.ReadCreationRules(root.FS())
		require.ErrorContains(t, err, ".sops.yaml: is a symlink")
	})
	t.Run("directory", func(t *testing.T) {
		t.Parallel()
		_, err := sops.ReadCreationRules(fstest.MapFS{".sops.yaml/x": {Data: body}})
		require.ErrorContains(t, err, ".sops.yaml: not a regular file")
	})
	t.Run("FS without Lstat fails closed", func(t *testing.T) {
		t.Parallel()
		_, err := sops.ReadCreationRules(openOnlyFS{fstest.MapFS{".sops.yaml": {Data: body}}})
		require.ErrorContains(t, err, ".sops.yaml: cannot tell whether it is a symlink")
	})
	t.Run("malformed", func(t *testing.T) {
		t.Parallel()
		_, err := sops.ReadCreationRules(fstest.MapFS{".sops.yaml": {Data: []byte("creation_rules: {")}})
		require.ErrorContains(t, err, ".sops.yaml:")
	})
}

// openOnlyFS hides every interface but fs.FS.
type openOnlyFS struct{ fsys fs.FS }

func (o openOnlyFS) Open(name string) (fs.File, error) { return o.fsys.Open(name) }
