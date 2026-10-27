package e2eruntime_test

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/e2eruntime"
)

const (
	testID       = "pe2e-0a1b2c3d"
	testUID      = 61234
	testHome     = "/home/" + testID
	testRuntime  = "/run/user/61234"
	testScenario = "TestE2EApplyDown"
	testRecord   = e2eruntime.StateRoot + "/" + testID + "/allocation"
)

type owner struct {
	uid  int
	mode fs.FileMode
}

// fakeHost is a Machine on which the runner allocated testID for
// testScenario and started the test process inside it.
type fakeHost struct {
	env       map[string]string
	uid       int
	users     map[int]e2eruntime.Account
	owners    map[string]owner
	files     map[string]string
	graphRoot string
}

func validHost() *fakeHost {
	return &fakeHost{
		env: map[string]string{
			e2eruntime.EnvAllocation:   testRecord,
			"HOME":                     testHome,
			"XDG_RUNTIME_DIR":          testRuntime,
			"DBUS_SESSION_BUS_ADDRESS": "unix:path=" + testRuntime + "/bus",
			"PODMAN_SOCKET":            testRuntime + "/podman/podman.sock",
			"TMPDIR":                   testHome + "/tmp",
		},
		uid: testUID,
		users: map[int]e2eruntime.Account{
			testUID: {Name: testID, Home: testHome, Comment: e2eruntime.AccountComment(testID)},
		},
		owners: map[string]owner{
			e2eruntime.StateRoot:                {0, fs.ModeDir | 0o755},
			e2eruntime.StateRoot + "/" + testID: {0, fs.ModeDir | 0o755},
			testRecord:                          {0, 0o644},
			testRuntime:                         {testUID, fs.ModeDir | 0o700},
			testRuntime + "/podman/podman.sock": {testUID, fs.ModeSocket | 0o660},
		},
		files: map[string]string{
			testRecord: "id=" + testID + "\nuser=" + testID + "\nuid=61234\nhome=" + testHome + "\nscenario=" + testScenario + "\n",
		},
		graphRoot: testHome + "/.local/share/containers/storage",
	}
}

func (h *fakeHost) probe() e2eruntime.Probe {
	return e2eruntime.Probe{
		Getenv: func(k string) string { return h.env[k] },
		Getuid: func() int { return h.uid },
		LookupUID: func(uid int) (e2eruntime.Account, error) {
			a, ok := h.users[uid]
			if !ok {
				return e2eruntime.Account{}, errors.New("unknown uid")
			}
			return a, nil
		},
		Owner: func(path string) (int, fs.FileMode, error) {
			o, ok := h.owners[path]
			if !ok {
				return 0, 0, fs.ErrNotExist
			}
			return o.uid, o.mode, nil
		},
		ReadFile: func(path string) ([]byte, error) {
			c, ok := h.files[path]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return []byte(c), nil
		},
		GraphRoot: func() (string, error) { return h.graphRoot, nil },
	}
}

func TestVerifyAcceptsTheAllocatedRuntime(t *testing.T) {
	t.Parallel()
	rt, err := e2eruntime.Verify(validHost().probe(), testScenario)
	require.NoError(t, err)
	assert.Equal(t, &e2eruntime.Runtime{
		ID:           testID,
		Scenario:     testScenario,
		UID:          testUID,
		Home:         testHome,
		RuntimeDir:   testRuntime,
		PodmanSocket: testRuntime + "/podman/podman.sock",
	}, rt)
}

func TestVerifyWithoutAllocationNamesTheRunner(t *testing.T) {
	t.Parallel()
	h := validHost()
	delete(h.env, e2eruntime.EnvAllocation)
	_, err := e2eruntime.Verify(h.probe(), testScenario)
	require.ErrorIs(t, err, e2eruntime.ErrNoAllocation)
	assert.ErrorContains(t, err, "task e2e")
}

//nolint:funlen // one table of unsafe runtimes
func TestVerifyRejectsUnsafeRuntimes(t *testing.T) {
	t.Parallel()
	const devHome = "/home/dev"
	tests := []struct {
		name   string
		mutate func(h *fakeHost)
		want   string
	}{
		{"record outside the state root", func(h *fakeHost) {
			h.env[e2eruntime.EnvAllocation] = "/tmp/" + testID + "/allocation"
		}, "allocation record"},
		{"record writable by the test user", func(h *fakeHost) {
			h.owners[testRecord] = owner{testUID, 0o644}
		}, "owned by root"},
		{"group-writable allocation dir", func(h *fakeHost) {
			h.owners[e2eruntime.StateRoot+"/"+testID] = owner{0, fs.ModeDir | 0o775}
		}, "writable"},
		{"symlinked record", func(h *fakeHost) {
			h.owners[testRecord] = owner{0, fs.ModeSymlink | 0o777}
		}, "regular file"},
		{"disposed allocation", func(h *fakeHost) {
			delete(h.files, testRecord)
			delete(h.owners, testRecord)
		}, "allocation record"},
		{"record of another allocation", func(h *fakeHost) {
			h.files[testRecord] = "id=pe2e-ffffffff\nuser=pe2e-ffffffff\nuid=61234\nhome=" + testHome + "\nscenario=" + testScenario + "\n"
		}, "id"},
		{"another scenario's allocation", func(h *fakeHost) {
			h.files[testRecord] = "id=" + testID + "\nuser=" + testID + "\nuid=61234\nhome=" + testHome + "\nscenario=TestE2EBuild\n"
		}, "scenario"},
		{"process of another user", func(h *fakeHost) {
			h.uid = 1000
			h.users[1000] = e2eruntime.Account{Name: "dev", Home: devHome}
		}, "uid"},
		{"account the runner did not create", func(h *fakeHost) {
			h.users[testUID] = e2eruntime.Account{Name: testID, Home: testHome, Comment: "someone"}
		}, "comment"},
		{"passwd home differs from the record", func(h *fakeHost) {
			h.users[testUID] = e2eruntime.Account{Name: testID, Home: devHome, Comment: e2eruntime.AccountComment(testID)}
		}, "home"},
		{"stale HOME", func(h *fakeHost) { h.env["HOME"] = devHome }, "HOME"},
		{"developer's runtime dir", func(h *fakeHost) { h.env["XDG_RUNTIME_DIR"] = "/run/user/1000" }, "XDG_RUNTIME_DIR"},
		{"runtime dir of another owner", func(h *fakeHost) {
			h.owners[testRuntime] = owner{1000, fs.ModeDir | 0o700}
		}, "XDG_RUNTIME_DIR"},
		{"developer's session bus", func(h *fakeHost) {
			h.env["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=/run/user/1000/bus"
		}, "DBUS_SESSION_BUS_ADDRESS"},
		{"developer's Podman socket", func(h *fakeHost) {
			h.env["PODMAN_SOCKET"] = "/run/user/1000/podman/podman.sock"
		}, "PODMAN_SOCKET"},
		{"socket not owned by the scenario user", func(h *fakeHost) {
			h.owners[testRuntime+"/podman/podman.sock"] = owner{1000, fs.ModeSocket | 0o660}
		}, "PODMAN_SOCKET"},
		{"remote Podman connection", func(h *fakeHost) { h.env["CONTAINER_HOST"] = "unix:///run/user/1000/podman/podman.sock" }, "CONTAINER_HOST"},
		{"redirected storage config", func(h *fakeHost) { h.env["CONTAINERS_STORAGE_CONF"] = devHome + "/storage.conf" }, "CONTAINERS_STORAGE_CONF"},
		{"redirected XDG data dir", func(h *fakeHost) { h.env["XDG_DATA_HOME"] = devHome + "/.local/share" }, "XDG_DATA_HOME"},
		{"TMPDIR outside the home", func(h *fakeHost) { h.env["TMPDIR"] = "/tmp" }, "TMPDIR"},
		{"Podman storage outside the home", func(h *fakeHost) {
			h.graphRoot = devHome + "/.local/share/containers/storage"
		}, "storage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := validHost()
			tt.mutate(h)
			_, err := e2eruntime.Verify(h.probe(), testScenario)
			require.ErrorIs(t, err, e2eruntime.ErrUnsafeRuntime)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

// Podman is asked for its store only once every other check passed: a
// process in the wrong runtime must not touch that runtime's store.
func TestVerifyDoesNotRunPodmanInAnUnverifiedRuntime(t *testing.T) {
	t.Parallel()
	h := validHost()
	h.env["HOME"] = "/home/dev"
	p := h.probe()
	p.GraphRoot = func() (string, error) {
		t.Fatal("podman ran in an unverified runtime")
		return "", nil
	}
	_, err := e2eruntime.Verify(p, testScenario)
	require.ErrorIs(t, err, e2eruntime.ErrUnsafeRuntime)
}
