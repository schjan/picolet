package agentcfg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/sops"
)

func TestParseSopsBlockRequiresIdentityPath(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("hostname: node-1\nsops: {}\n"))
	require.ErrorContains(t, err, "sops.age_identity_path is required")

	_, err = Parse([]byte("hostname: node-1\nsops:\n  age_identity: AGE-SECRET-KEY-1\n"))
	require.Error(t, err, "no inline key")

	cfg, err := Parse([]byte("hostname: node-1\nsops:\n  age_identity_path: /etc/picolet/secrets/age-identity\n"))
	require.NoError(t, err)
	assert.Equal(t, "/etc/picolet/secrets/age-identity", cfg.Sops.AgeIdentityPath)

	cfg, err = Parse([]byte("hostname: node-1\n"))
	require.NoError(t, err)
	assert.Nil(t, cfg.Sops, "no sops block, no default path")
}

func TestLoadSopsIdentities(t *testing.T) {
	t.Parallel()
	var none Config
	ids, loose, err := none.LoadSopsIdentities()
	require.NoError(t, err)
	assert.Nil(t, ids)
	assert.False(t, loose)

	dir := t.TempDir()
	path := filepath.Join(dir, "age-identity")
	both := testsops.IdentityFile(t, "srv-1", "srv-2")
	require.NoError(t, os.WriteFile(path, both, 0o600))
	cfg := Config{Sops: &SopsConfig{AgeIdentityPath: path}}

	ids, loose, err = cfg.LoadSopsIdentities()
	require.NoError(t, err)
	assert.Equal(t, 2, ids.Len(), "one file may hold several identities, for rotation")
	assert.False(t, loose)

	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		require.NoError(t, os.Chmod(path, mode))
		_, loose, err = cfg.LoadSopsIdentities()
		require.NoError(t, err)
		assert.True(t, loose, "mode %o is readable by group or others", mode)
	}
}

func TestLoadSopsIdentitiesBrokenFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "unreadable")
	// A directory: unreadable even for root, which a chmod 000 file is not.
	require.NoError(t, os.Mkdir(unreadable, 0o700))
	garbage := filepath.Join(dir, "garbage")
	require.NoError(t, os.WriteFile(garbage, []byte("TEST-ONLY-not-a-key\n"), 0o600))
	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, []byte("# no identity yet\n\n"), 0o600))

	tests := []struct {
		name string
		path string
		want error
	}{
		{name: "missing", path: filepath.Join(dir, "missing")},
		{name: "unreadable", path: unreadable},
		{name: "garbage", path: garbage, want: sops.ErrBadIdentity},
		{name: "no identity", path: empty, want: sops.ErrNoIdentity},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{Sops: &SopsConfig{AgeIdentityPath: tc.path}}
			_, _, err := cfg.LoadSopsIdentities()
			require.Error(t, err)
			require.ErrorContains(t, err, "sops.age_identity_path")
			require.ErrorContains(t, err, tc.path)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
			assert.NotContains(t, err.Error(), "TEST-ONLY-not-a-key", "never quote the identity file")

			// The startup checks, and with them the self-restart guard,
			// refuse the same file.
			cfg.Hostname, cfg.RepoURL = "node-1", "https://example.com/fleet.git"
			_, err = cfg.CheckStartup()
			require.Error(t, err)
		})
	}
}

func TestCheckRenderedRejectsMissingIdentityFile(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "age-identity")
	err := CheckRenderedConfig([]byte("hostname: node-1\nrepo_url: https://example.com/fleet.git\nsops:\n  age_identity_path: "+missing+"\n"), nil)
	require.ErrorContains(t, err, "sops.age_identity_path")
	require.ErrorContains(t, err, missing)
}

func TestCheckStartupAcceptsIdentityFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "age-identity")
	require.NoError(t, os.WriteFile(path, testsops.IdentityFile(t, "srv-1"), 0o600))
	err := CheckRenderedConfig([]byte("hostname: node-1\nrepo_url: https://example.com/fleet.git\nsops:\n  age_identity_path: "+path+"\n"), nil)
	require.NoError(t, err)
}

func TestLoadRejectsIdentityFileSharedWithAnotherCredential(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "age-identity")
	require.NoError(t, os.WriteFile(path, testsops.IdentityFile(t, "srv-1"), 0o600))
	cfgPath := filepath.Join(dir, "config.yml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("hostname: node-1\ngit_token_path: "+path+"\nsops:\n  age_identity_path: "+path+"\n"), 0o600))
	_, err := Load(cfgPath)
	require.ErrorContains(t, err, "same credential file")
}
