package sops_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/getsops/sops/v3/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/sops"
)

// inspectSOPS inspects a corpus file that must be a SOPS file.
func inspectSOPS(t *testing.T, name string) *sops.File {
	t.Helper()
	f := inspectCorpus(t, name)
	require.True(t, f.IsSOPS, f.NotSOPSReason)
	return f
}

// rulesOf reads cfg as the Fleet-root `.sops.yaml`.
func rulesOf(t *testing.T, cfg string) *sops.CreationRules {
	t.Helper()
	rules, err := sops.ReadCreationRules(fstest.MapFS{".sops.yaml": {Data: []byte(cfg)}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rules.Close() })
	return rules
}

type checkRecipientsCase struct {
	name, file, cfg string
	// want is the error's text after the path; empty passes.
	want string
}

func checkRecipientsCases(t *testing.T) []checkRecipientsCase {
	t.Helper()
	srv1, srv2, srv3 := testsops.Recipient(t, "srv-1"), testsops.Recipient(t, "srv-2"), testsops.Recipient(t, "srv-3")
	op, pq := testsops.Recipient(t, "operator-1"), testsops.Recipient(t, "srv-pq")
	const update = "run `sops updatekeys secrets/db.sops.yaml`"

	return []checkRecipientsCase{
		{
			name: "equal, other order, CSV string",
			file: "values.sops.yaml",
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age: " + op + ", " + srv2 + "," + srv1 + "\n",
		},
		{
			name: "equal, YAML list",
			file: "values.sops.yaml",
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age:\n      - " + srv1 + "\n      - " + srv2 + "\n      - " + op + "\n",
		},
		{
			name: "file has extra and lacks one",
			file: "values.sops.yaml",
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age: " + srv1 + "," + srv2 + "," + srv3 + "\n",
			want: "recipients differ from the `.sops.yaml` creation rule matching it: +" + op + " −" + srv3 + "; " + update,
		},
		{
			name: "empty path_regex",
			file: "values.sops.json",
			cfg:  "creation_rules:\n  - age: " + srv1 + "\n",
			want: "recipients differ from the `.sops.yaml` creation rule matching it: +" + srv2 + "; " + update,
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
			want: "recipients differ from the `.sops.yaml` creation rule matching it: key group 1: +" + op + "; key group 2: −" + op + "; " + update,
		},
		{
			name: "key groups vs single group rule",
			file: "key-groups.sops.yaml",
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age: " + srv1 + "," + op + "\n",
			want: "recipients differ from the `.sops.yaml` creation rule matching it: file has 3 key groups, rule has 1; key group 2: +" + srv2 + "; key group 3: +" + pq + "; " + update,
		},
		{
			name: "non-age keys ignored",
			file: "non-age.sops.yaml",
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age: " + srv1 + "," + srv2 + "\n    pgp: \"0000000000000000000000000000000000000000\"\n",
		},
	}
}

func TestCheckRecipients(t *testing.T) {
	t.Parallel()
	for _, tt := range checkRecipientsCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := rulesOf(t, tt.cfg).Check("secrets/db.sops.yaml", inspectSOPS(t, tt.file))
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, "`secrets/db.sops.yaml` "+tt.want)
		})
	}
}

// A file no rule matches, or a .sops.yaml without creation_rules, is not
// checked.
func TestCheckUnchecked(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]string{
		"no matching rule":  "creation_rules:\n  - path_regex: ^other/\n    age: " + testsops.Recipient(t, "srv-3") + "\n",
		"no creation_rules": "destination_rules: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, rulesOf(t, cfg).Check("secrets/db.sops.yaml", inspectSOPS(t, "values.sops.yaml")))
		})
	}
}

func TestCheckInvalidRule(t *testing.T) {
	t.Parallel()
	srv1 := testsops.Recipient(t, "srv-1")
	for name, tt := range map[string]struct{ cfg, want string }{
		"bad path_regex": {
			cfg:  "creation_rules:\n  - path_regex: \"(\"\n    age: " + srv1 + "\n",
			want: "can not compile regexp",
		},
		"malformed recipient": {
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age: age1nope\n",
			want: "failed to parse input as Bech32-encoded age public key",
		},
		"upper case recipient, as sops rejects it": {
			cfg:  "creation_rules:\n  - age: " + strings.ToUpper(srv1) + "\n",
			want: "unknown recipient type",
		},
		"age not strings": {
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age: [1, 2]\n",
			want: "invalid age key configuration",
		},
		"two encryption rules": {
			cfg:  "creation_rules:\n  - age: " + srv1 + "\n    encrypted_regex: x\n    unencrypted_regex: y\n",
			want: "cannot use more than one of",
		},
		"malformed YAML": {
			cfg:  "creation_rules: {",
			want: "the file does not decode",
		},
		"age identity is never echoed": {
			cfg:  "creation_rules:\n  - age: " + zeroIdentity + "\n",
			want: "an age identity (secret key) is not an age recipient; use its public key",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := rulesOf(t, tt.cfg).Check("secrets/db.sops.yaml", inspectSOPS(t, "values.sops.yaml"))
			require.ErrorIs(t, err, sops.ErrInvalidCreationRule)
			require.ErrorContains(t, err, "`.sops.yaml` rule for `secrets/db.sops.yaml` is invalid: ")
			require.ErrorContains(t, err, tt.want)
			assert.NotContains(t, strings.ToUpper(err.Error()), zeroIdentity)
		})
	}
}

// nonAgeBackends are creation-rule key entries of every non-age backend,
// pointed at unroutable or unreachable endpoints: a versionless Azure Key
// Vault URL (which upstream would resolve to its latest version over the
// network), an HC Vault transit URI, a GCP KMS key, an AWS KMS ARN, a
// Huawei Cloud KMS key and a PGP fingerprint.
const (
	nonAgeTopLevel = "    azure_keyvault: https://unroutable.vault.azure.net/keys/k\n" +
		"    hc_vault_transit_uri: http://10.255.255.1:8200/v1/sops/keys/k\n" +
		"    gcp_kms: projects/p/locations/global/keyRings/r/cryptoKeys/k\n" +
		"    kms: arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000\n" +
		"    aws_profile: unroutable\n" +
		"    hckms: [\"tr-west-1:00000000-0000-0000-0000-000000000000\"]\n" +
		"    pgp: \"0000000000000000000000000000000000000000\"\n"
	nonAgeGroup = "        azure_keyvault: [{vaultUrl: \"https://unroutable.vault.azure.net\", key: k}]\n" +
		"        hc_vault: [\"http://10.255.255.1:8200/v1/sops/keys/k\"]\n" +
		"        gcp_kms: [{resource_id: projects/p/locations/global/keyRings/r/cryptoKeys/k}]\n" +
		"        kms: [{arn: \"arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000\", aws_profile: unroutable}]\n" +
		"        hckms: [{key_id: \"tr-west-1:00000000-0000-0000-0000-000000000000\"}]\n" +
		"        pgp: [\"0000000000000000000000000000000000000000\"]\n"
)

// Check compares age Recipients only, and never constructs a non-age key
// backend for that: a rule naming them next to matching age Recipients, at
// the top level or in key_groups, passes at once, without credentials or a
// network lookup. A rule left with no age Recipients is still the one that
// matches, and compares as one empty group.
func TestCheckNeverBuildsNonAgeBackends(t *testing.T) {
	t.Parallel()
	srv1, srv2, op := testsops.Recipient(t, "srv-1"), testsops.Recipient(t, "srv-2"), testsops.Recipient(t, "operator-1")
	pq := testsops.Recipient(t, "srv-pq")
	for _, tt := range []struct {
		name, file, cfg, want string
	}{
		{
			name: "top level, next to age",
			file: "values.sops.yaml",
			cfg:  "creation_rules:\n  - path_regex: ^secrets/\n    age: " + srv1 + "," + srv2 + "," + op + "\n" + nonAgeTopLevel,
		},
		{
			name: "versionless Azure URL in key_groups, next to age",
			file: "key-groups.sops.yaml",
			cfg: "creation_rules:\n  - path_regex: ^secrets/\n    key_groups:\n" +
				"      - age: [" + op + ", " + srv1 + "]\n" + nonAgeGroup +
				"      - age: [" + srv2 + "]\n" +
				"      - merge:\n          - age: [" + pq + "]\n" + strings.ReplaceAll(nonAgeGroup, "        ", "            "),
		},
		{
			name: "anchors and merge keys, as upstream resolves them",
			file: "values.sops.yaml",
			cfg: "defaults: &defaults\n  age: " + srv1 + "," + srv2 + "," + op + "\n" +
				"  azure_keyvault: https://unroutable.vault.azure.net/keys/k\n" +
				"creation_rules:\n  - <<: *defaults\n    path_regex: ^secrets/\n",
		},
		{
			name: "rule without age, first match",
			file: "values.sops.yaml",
			cfg: "creation_rules:\n  - path_regex: ^secrets/\n" + nonAgeTopLevel +
				"  - path_regex: ^secrets/\n    age: " + srv1 + "," + srv2 + "," + op + "\n",
			want: "recipients differ",
		},
		{
			name: "groups without age keep their count",
			file: "key-groups.sops.yaml",
			cfg: "creation_rules:\n  - path_regex: ^secrets/\n    key_groups:\n" +
				"      - age: [" + op + ", " + srv1 + "]\n      - age: [" + srv2 + "]\n" +
				"      - azure_keyvault: [{vaultUrl: \"https://unroutable.vault.azure.net\", key: k}]\n",
			want: "key group 3: +" + pq,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			err := rulesOf(t, tt.cfg).Check("secrets/db.sops.yaml", inspectSOPS(t, tt.file))
			assert.Less(t, time.Since(start), 2*time.Second, "Check waited on a key backend")
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.want)
			require.NotErrorIs(t, err, sops.ErrInvalidCreationRule)
		})
	}
}

// TestNoMatchingRuleText pins the upstream error text Check reads as "no
// rule matches": a SOPS upgrade that changes it must fail here, not turn
// every unmatched file into an invalid-rule error.
func TestNoMatchingRuleText(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	conf := filepath.Join(dir, ".sops.yaml")
	require.NoError(t, os.WriteFile(conf, []byte("creation_rules:\n  - path_regex: ^other/\n"), 0o600))
	_, err := config.LoadCreationRuleForFile(conf, filepath.Join(dir, "secrets", "db.sops.yaml"), nil)
	require.EqualError(t, err, sops.NoMatchingRule)
}

func TestReadCreationRules(t *testing.T) {
	t.Parallel()
	body := []byte("creation_rules:\n  - path_regex: ^secrets/\n")
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		rules, err := sops.ReadCreationRules(fstest.MapFS{})
		require.NoError(t, err)
		assert.Empty(t, sops.RulesDir(rules))
		require.NoError(t, rules.Check("secrets/db.sops.yaml", inspectSOPS(t, "values.sops.yaml")))
		require.NoError(t, rules.Close())
	})
	t.Run("regular file, private copy removed on Close", func(t *testing.T) {
		t.Parallel()
		rules, err := sops.ReadCreationRules(fstest.MapFS{".sops.yaml": {Data: body}})
		require.NoError(t, err)
		dir := sops.RulesDir(rules)
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o700), info.Mode().Perm())
		err = rules.Check("secrets/db.sops.yaml", inspectSOPS(t, "values.sops.yaml"))
		require.ErrorContains(t, err, "recipients differ")
		require.NoError(t, rules.Close())
		assert.NoDirExists(t, dir)
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
}

// openOnlyFS hides every interface but fs.FS.
type openOnlyFS struct{ fsys fs.FS }

func (o openOnlyFS) Open(name string) (fs.File, error) { return o.fsys.Open(name) }
