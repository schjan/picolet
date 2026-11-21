// Package e2eruntime verifies that an E2E test process runs inside the
// disposable runtime the E2E runner (scripts/e2e.sh) allocated for its
// scenario: a Linux user the runner created, with its own home, user systemd
// manager and Podman store. A runtime test calls Verify before touching
// systemd or Podman and refuses to run when it fails, so a direct `go test`
// or a stale environment never reaches the invoking user's runtime.
package e2eruntime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	// StateRoot holds one root-owned directory per allocation, named by its
	// id, with the allocation record the runner writes before it creates the
	// user. scripts/e2e.sh uses the same path.
	StateRoot = "/var/lib/picolet-e2e"
	// EnvAllocation names the allocation record of the test process.
	EnvAllocation = "PICOLET_E2E_ALLOCATION"
	// recordName is the record's file name in its allocation directory.
	recordName = "allocation"
	// idPrefix starts every allocation id, which is also the user's name.
	idPrefix = "pe2e-"
)

var (
	// ErrNoAllocation reports a test process the runner did not start.
	ErrNoAllocation = errors.New("no E2E runtime allocation: run the E2E suite through `task e2e` (scripts/e2e.sh), " +
		"which runs each scenario as its own disposable user")
	// ErrUnsafeRuntime reports a process whose environment or accounts do
	// not match its allocation record.
	ErrUnsafeRuntime = errors.New("unsafe E2E runtime")
)

// AccountComment is the passwd comment (GECOS) the runner gives the user it
// creates for allocation id, so the guard and the runner's recovery only
// accept an account the runner made.
func AccountComment(id string) string { return "picolet-e2e " + id }

// Account is a passwd entry.
type Account struct {
	Name    string
	Home    string
	Comment string
}

// Probe is what Verify reads from the process and the Machine.
type Probe struct {
	Getenv    func(key string) string
	Getuid    func() int
	LookupUID func(uid int) (Account, error)
	// Owner returns path's owner and mode without following a symlink.
	Owner    func(path string) (uid int, mode fs.FileMode, err error)
	ReadFile func(path string) ([]byte, error)
	// GraphRoot returns the Podman store the process's podman uses.
	GraphRoot func() (string, error)
}

// OS returns the Probe of this process.
func OS() Probe {
	return Probe{
		Getenv: os.Getenv,
		Getuid: os.Getuid,
		LookupUID: func(uid int) (Account, error) {
			u, err := user.LookupId(strconv.Itoa(uid))
			if err != nil {
				return Account{}, err
			}
			return Account{Name: u.Username, Home: u.HomeDir, Comment: u.Name}, nil
		},
		Owner: func(path string) (int, fs.FileMode, error) {
			fi, err := os.Lstat(path)
			if err != nil {
				return 0, 0, err
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, 0, fmt.Errorf("%s: no owner", path)
			}
			return int(st.Uid), fi.Mode(), nil
		},
		ReadFile: os.ReadFile,
		GraphRoot: func() (string, error) {
			out, err := exec.Command("podman", "info", "--format", "{{.Store.GraphRoot}}").Output()
			if err != nil {
				return "", fmt.Errorf("podman info: %w", err)
			}
			return strings.TrimSpace(string(out)), nil
		},
	}
}

// Runtime is a verified allocation.
type Runtime struct {
	ID       string
	Scenario string
	UID      int
	Home     string
	// RuntimeDir is the user's XDG_RUNTIME_DIR.
	RuntimeDir   string
	PodmanSocket string
}

// redirects are variables that would point Podman or the resolver's
// destinations outside the allocated runtime; the runner sets none of them.
var redirects = []string{
	"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE",
	"CONTAINERS_STORAGE_CONF", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
}

// Verify checks that the process runs as the user the runner allocated for
// scenario, with that user's home, runtime directory, session bus, Podman
// socket and store, and returns the runtime. Podman is run only after every
// other check passed.
func Verify(p Probe, scenario string) (*Runtime, error) {
	recordPath := p.Getenv(EnvAllocation)
	if recordPath == "" {
		return nil, ErrNoAllocation
	}
	rec, err := readRecord(p, recordPath)
	if err != nil {
		return nil, unsafeRuntime(err)
	}
	rt := &Runtime{ID: rec["id"], Scenario: rec["scenario"], Home: rec["home"]}
	if rt.UID, err = strconv.Atoi(rec["uid"]); err != nil || rt.UID <= 0 {
		return nil, unsafeRuntime(fmt.Errorf("allocation record %s: invalid uid %q", recordPath, rec["uid"]))
	}
	rt.RuntimeDir = "/run/user/" + strconv.Itoa(rt.UID)
	rt.PodmanSocket = rt.RuntimeDir + "/podman/podman.sock"
	if err := errors.Join(checkIdentity(p, rec, rt, scenario), checkEnv(p, rt)); err != nil {
		return nil, unsafeRuntime(err)
	}
	graphRoot, err := p.GraphRoot()
	if err != nil {
		return nil, unsafeRuntime(err)
	}
	if !within(rt.Home, graphRoot) {
		return nil, unsafeRuntime(fmt.Errorf("podman storage %s is outside the allocated home %s", graphRoot, rt.Home))
	}
	return rt, nil
}

func unsafeRuntime(err error) error { return fmt.Errorf("%w: %w", ErrUnsafeRuntime, err) }

// readRecord reads the allocation record at path, which must be
// StateRoot/<id>/allocation, root-owned and writable by root only all the
// way down from StateRoot, and name its own id.
func readRecord(p Probe, path string) (map[string]string, error) {
	dir := filepath.Dir(path)
	id := filepath.Base(dir)
	if path != filepath.Join(StateRoot, id, recordName) || !strings.HasPrefix(id, idPrefix) {
		return nil, fmt.Errorf("allocation record %s is not %s/%s<id>/%s", path, StateRoot, idPrefix, recordName)
	}
	for _, c := range []struct {
		path string
		typ  fs.FileMode
	}{{StateRoot, fs.ModeDir}, {dir, fs.ModeDir}, {path, 0}} {
		if err := checkRootOwned(p, c.path, c.typ); err != nil {
			return nil, fmt.Errorf("allocation record: %w", err)
		}
	}
	data, err := p.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("allocation record: %w", err)
	}
	rec := map[string]string{}
	for line := range strings.Lines(string(data)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			rec[k] = v
		}
	}
	if rec["id"] != id {
		return nil, fmt.Errorf("allocation record %s names id %q", path, rec["id"])
	}
	return rec, nil
}

func checkRootOwned(p Probe, path string, typ fs.FileMode) error {
	uid, mode, err := p.Owner(path)
	if err != nil {
		return err
	}
	if mode.Type() != typ {
		want := "regular file"
		if typ == fs.ModeDir {
			want = "directory"
		}
		return fmt.Errorf("%s is not a %s", path, want)
	}
	if uid != 0 {
		return fmt.Errorf("%s must be owned by root, is owned by uid %d", path, uid)
	}
	if mode.Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (%s)", path, mode.Perm())
	}
	return nil
}

// checkIdentity checks the process and its passwd entry against the record.
func checkIdentity(p Probe, rec map[string]string, rt *Runtime, scenario string) error {
	var errs []error
	if rt.Scenario != scenario {
		errs = append(errs, fmt.Errorf("allocation is for scenario %q, not %q", rt.Scenario, scenario))
	}
	if uid := p.Getuid(); uid != rt.UID {
		return errors.Join(append(errs, fmt.Errorf("process uid %d is not the allocated uid %d", uid, rt.UID))...)
	}
	acct, err := p.LookupUID(rt.UID)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("looking up uid %d: %w", rt.UID, err))...)
	}
	if acct.Name != rec["user"] || acct.Name != rt.ID {
		errs = append(errs, fmt.Errorf("uid %d is %q, not the allocated user %q", rt.UID, acct.Name, rt.ID))
	}
	if acct.Comment != AccountComment(rt.ID) {
		errs = append(errs, fmt.Errorf("user %q has comment %q: not created by the E2E runner", acct.Name, acct.Comment))
	}
	if acct.Home != rt.Home || !filepath.IsAbs(rt.Home) {
		errs = append(errs, fmt.Errorf("passwd home %q is not the allocated home %q", acct.Home, rt.Home))
	}
	return errors.Join(errs...)
}

// checkEnv checks that the environment selects the allocated runtime.
func checkEnv(p Probe, rt *Runtime) error {
	var errs []error
	for _, want := range []struct{ key, value string }{
		{"HOME", rt.Home},
		{"XDG_RUNTIME_DIR", rt.RuntimeDir},
		{"DBUS_SESSION_BUS_ADDRESS", "unix:path=" + rt.RuntimeDir + "/bus"},
		{"PODMAN_SOCKET", rt.PodmanSocket},
	} {
		if got := p.Getenv(want.key); got != want.value {
			errs = append(errs, fmt.Errorf("%s=%q, want %q", want.key, got, want.value))
		}
	}
	for _, k := range redirects {
		if v := p.Getenv(k); v != "" {
			errs = append(errs, fmt.Errorf("%s=%q must be unset", k, v))
		}
	}
	if tmp := p.Getenv("TMPDIR"); !within(rt.Home, tmp) {
		errs = append(errs, fmt.Errorf("TMPDIR=%q is outside the allocated home %s", tmp, rt.Home))
	}
	errs = append(errs,
		checkUserOwned(p, "XDG_RUNTIME_DIR", rt.RuntimeDir, fs.ModeDir, rt.UID),
		checkUserOwned(p, "PODMAN_SOCKET", rt.PodmanSocket, fs.ModeSocket, rt.UID))
	return errors.Join(errs...)
}

func checkUserOwned(p Probe, name, path string, typ fs.FileMode, uid int) error {
	got, mode, err := p.Owner(path)
	switch {
	case err != nil:
		return fmt.Errorf("%s %s: %w", name, path, err)
	case mode.Type() != typ:
		return fmt.Errorf("%s %s has type %s", name, path, mode.Type())
	case got != uid:
		return fmt.Errorf("%s %s is owned by uid %d, not %d", name, path, got, uid)
	case typ == fs.ModeDir && mode.Perm()&0o077 != 0:
		return fmt.Errorf("%s %s is accessible to others (%s)", name, path, mode.Perm())
	}
	return nil
}

// within reports whether path is dir or below it.
func within(dir, path string) bool {
	if dir == "" || !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
