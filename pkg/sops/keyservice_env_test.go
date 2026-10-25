package sops_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/sops"
)

// TestReaderIgnoresAmbientKeys plants srv-2's key everywhere SOPS's own age
// decryption looks when it holds no identity (SOPS_AGE_KEY, SOPS_AGE_KEY_FILE,
// the _CMD variables, keys.txt under XDG_CONFIG_HOME, ~/.ssh) and points every
// proxy and AWS endpoint at a listener. Only the identities passed in may be
// used, no command may run and nothing may connect.
//
// t.Setenv rules out t.Parallel(), so this test has no subtests.
func TestReaderIgnoresAmbientKeys(t *testing.T) {
	marker := plantAmbientKeys(t)
	connections := trapNetwork(t)

	fsys := os.DirFS(corpusDir)

	// values.sops.json is encrypted to srv-1 and srv-2. With srv-3 passed in,
	// srv-2's key in the environment must not open it.
	_, err := sops.NewReader(fsys, corpusIdentities(t, "srv-3")).ReadAll(t.Context(),
		[]string{"sops://corpus/values.sops.json#/db/password"})
	require.ErrorIs(t, err, sops.ErrNotRecipient)

	// No identity configured: the environment is no fallback either.
	_, err = sops.NewReader(fsys, nil).ReadAll(t.Context(),
		[]string{"sops://corpus/values.sops.json#/db/password"})
	require.ErrorIs(t, err, sops.ErrNoIdentity)

	// The identities passed in still work, and the KMS/PGP stanzas next to
	// the age ones are never tried against a backend.
	refs := []string{
		"sops://corpus/values.sops.json#/db/password",
		"sops://corpus/non-age.sops.yaml#/db/password",
	}
	got, err := sops.NewReader(fsys, corpusIdentities(t, "srv-1")).ReadAll(t.Context(), refs)
	require.NoError(t, err)
	require.Equal(t, map[string]string{refs[0]: "hunter2-test", refs[1]: "hunter2-test"}, got)

	require.NoFileExists(t, marker, "a SOPS_AGE_*_CMD command ran")
	require.Zero(t, connections.Load(), "decryption attempted a network connection")
}

// plantAmbientKeys puts srv-2's key everywhere SOPS's own age decryption
// looks and returns the marker file its _CMD command leaves if it runs.
func plantAmbientKeys(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	srv2KeyFile, err := filepath.Abs(filepath.Join(corpusDir, "identities", "srv-2.agekey"))
	require.NoError(t, err)
	srv2Key, err := os.ReadFile(srv2KeyFile)
	require.NoError(t, err)

	root, err := os.OpenRoot(tmp)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })

	// A command that leaves a marker (and offers srv-2's key) if it runs.
	marker := filepath.Join(tmp, "command-ran")
	cmd := filepath.Join(tmp, "key-cmd.sh")
	script := "#!/bin/sh\necho ran >> '" + marker + "'\ncat '" + srv2KeyFile + "'\n"
	writeFile(t, root, "key-cmd.sh", []byte(script))
	require.NoError(t, root.Chmod("key-cmd.sh", 0o700))

	home := filepath.Join(tmp, "home")
	xdg := filepath.Join(tmp, "xdg")
	sshKey := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nTEST-ONLY\n-----END OPENSSH PRIVATE KEY-----\n")
	writeFile(t, root, "xdg/sops/age/keys.txt", srv2Key)
	writeFile(t, root, "home/.ssh/id_ed25519", sshKey)
	writeFile(t, root, "home/.ssh/id_rsa", sshKey)

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("SOPS_AGE_KEY", string(srv2Key))
	t.Setenv("SOPS_AGE_KEY_FILE", srv2KeyFile)
	t.Setenv("SOPS_AGE_KEY_CMD", cmd)
	t.Setenv("SOPS_AGE_SSH_PRIVATE_KEY_FILE", filepath.Join(home, ".ssh", "id_ed25519"))
	t.Setenv("SOPS_AGE_SSH_PRIVATE_KEY_CMD", cmd)
	return marker
}

// trapNetwork points every proxy and key-backend endpoint variable at a
// local listener and returns its connection count.
func trapNetwork(t *testing.T) *atomic.Int32 {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	var connections atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if err == nil {
				connections.Add(1)
				_ = c.Close()
			}
		}
	}()
	endpoint := "http://" + ln.Addr().String()
	for _, v := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy",
		"AWS_ENDPOINT_URL", "AWS_EC2_METADATA_SERVICE_ENDPOINT", "VAULT_ADDR",
	} {
		t.Setenv(v, endpoint)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	return &connections
}

// writeFile writes data at name below root, creating its parents.
func writeFile(t *testing.T, root *os.Root, name string, data []byte) {
	t.Helper()
	require.NoError(t, root.MkdirAll(filepath.Dir(name), 0o700))
	require.NoError(t, root.WriteFile(name, data, 0o600))
}
