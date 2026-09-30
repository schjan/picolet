package machine

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// machineRoot builds a Machine filesystem under a temp dir: path → content,
// a trailing "/" makes a directory. Every entry gets the given mode.
func machineRoot(t *testing.T, entries map[string]fs.FileMode, files map[string]string) *OSHostOps {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o755), "the Machine's / is searchable by everyone")
	for p, content := range files {
		full := filepath.Join(root, p)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644)) //nolint:gosec // world-readable files are what WorldReadableTree inspects
	}
	for p, mode := range entries {
		full := filepath.Join(root, p)
		require.NoError(t, os.MkdirAll(full, 0o755))
		require.NoError(t, os.Chmod(full, mode))
	}
	return &OSHostOps{root: root, euid: os.Geteuid()}
}

func TestOSHostOpsSubIDRanges(t *testing.T) {
	t.Parallel()
	ops := machineRoot(t, nil, map[string]string{
		"/etc/subuid": "# comment\npi:100000:65536\nrunner:165536:0\n1002:231072:65536\n" +
			"bad:not-a-number:65536\nlast:4294967295:1\nover:4294967295:2\n",
		"/etc/subgid": "pi:100000:65536\nbad:100000:65536\n",
	})
	tests := []struct {
		user           User
		subuid, subgid bool
	}{
		{User{Name: "pi", UID: 1000}, true, true},
		{User{Name: "runner", UID: 1001}, false, false},
		{User{Name: "ci", UID: 1002}, true, false},
		{User{Name: "nobody", UID: 65534}, false, false},
		{User{Name: "bad", UID: 1003}, false, true},
		{User{Name: "last", UID: 1004}, true, false},
		{User{Name: "over", UID: 1005}, false, false},
	}
	for _, tt := range tests {
		subuid, subgid, err := ops.SubIDRanges(tt.user)
		require.NoError(t, err)
		assert.Equal(t, tt.subuid, subuid, "%s subuid", tt.user.Name)
		assert.Equal(t, tt.subgid, subgid, "%s subgid", tt.user.Name)
	}

	none := machineRoot(t, nil, nil)
	subuid, subgid, err := none.SubIDRanges(User{Name: "pi", UID: 1000})
	require.NoError(t, err)
	assert.False(t, subuid || subgid, "no subuid/subgid files: no ranges")
}

func TestOSHostOpsSessionState(t *testing.T) {
	t.Parallel()
	ops := machineRoot(t, nil, map[string]string{
		"/var/lib/systemd/linger/pi":     "",
		"/run/user/1000/systemd/private": "",
	})
	pi, runner := User{Name: "pi", UID: 1000}, User{Name: "runner", UID: 1001}

	for _, tt := range []struct {
		name  string
		check func(User) (bool, error)
	}{{"linger", ops.LingerEnabled}, {"user manager", ops.UserManagerRunning}} {
		ok, err := tt.check(pi)
		require.NoError(t, err)
		assert.True(t, ok, "%s of pi", tt.name)
		ok, err = tt.check(runner)
		require.NoError(t, err)
		assert.False(t, ok, "%s of runner", tt.name)
	}
}

func TestOSHostOpsStat(t *testing.T) {
	t.Parallel()
	ops := machineRoot(t, map[string]fs.FileMode{"/home/pi/.config": 0o750}, nil)

	info, err := ops.Stat("/home/pi/.config")
	require.NoError(t, err)
	assert.Equal(t, PathInfo{Exists: true, UID: os.Getuid(), GID: os.Getgid(), Mode: fs.ModeDir | 0o750}, info)

	info, err = ops.Stat("/home/pi/.local")
	require.NoError(t, err)
	assert.False(t, info.Exists)
}

func TestOSHostOpsUnprivilegedIsUnknown(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads every home")
	}
	ops := machineRoot(t, map[string]fs.FileMode{"/home/runner": 0o700}, map[string]string{"/home/runner/.config/x": ""})
	require.NoError(t, os.Chmod(filepath.Join(ops.root, "/home/runner"), 0))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(ops.root, "/home/runner"), 0o700) })

	_, err := ops.Stat("/home/runner/.config")
	require.ErrorIs(t, err, ErrUnprivileged)
}

func TestOSHostOpsFileContentEquals(t *testing.T) {
	t.Parallel()
	ops := machineRoot(t, nil, map[string]string{"/etc/picolet/secrets/git-token": "token"})

	for _, tt := range []struct {
		path, content string
		want          bool
	}{
		{"/etc/picolet/secrets/git-token", "token", true},
		{"/etc/picolet/secrets/git-token", "rotated", false},
		{"/etc/picolet/secrets/missing", "token", false},
		{"/etc/picolet/secrets", "token", false},
	} {
		equal, err := ops.FileContentEquals(tt.path, []byte(tt.content))
		require.NoError(t, err)
		assert.Equal(t, tt.want, equal, "%s = %q", tt.path, tt.content)
	}
}

func TestOSHostOpsWorldReadableTree(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		entries map[string]fs.FileMode
		want    bool
	}{
		{"world-readable", map[string]fs.FileMode{"/srv": 0o755, "/srv/fleet": 0o755, "/srv/fleet/hosts": 0o755}, true},
		{"private subdirectory", map[string]fs.FileMode{"/srv": 0o755, "/srv/fleet": 0o755, "/srv/fleet/hosts": 0o750}, false},
		{"ancestor not searchable", map[string]fs.FileMode{"/srv": 0o750, "/srv/fleet": 0o755, "/srv/fleet/hosts": 0o755}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ops := machineRoot(t, nil, map[string]string{"/srv/fleet/fleet.yml": ""})
			for p, mode := range tt.entries {
				full := filepath.Join(ops.root, p)
				require.NoError(t, os.MkdirAll(full, 0o755))
				require.NoError(t, os.Chmod(full, mode))
			}
			readable, err := ops.WorldReadableTree("/srv/fleet")
			require.NoError(t, err)
			assert.Equal(t, tt.want, readable)
		})
	}

	ops := machineRoot(t, map[string]fs.FileMode{"/srv": 0o755}, map[string]string{"/srv/fleet/secret.txt": ""})
	require.NoError(t, os.Chmod(filepath.Join(ops.root, "/srv/fleet/secret.txt"), 0o600))
	readable, err := ops.WorldReadableTree("/srv/fleet")
	require.NoError(t, err)
	assert.False(t, readable, "a file others cannot read")
}

func TestOSHostOpsWorldReadableTreeMissing(t *testing.T) {
	t.Parallel()
	ops := machineRoot(t, map[string]fs.FileMode{"/srv": 0o755}, nil)
	readable, err := ops.WorldReadableTree("/srv/fleet")
	require.NoError(t, err)
	assert.False(t, readable, "a checkout that does not exist is not readable")
}
