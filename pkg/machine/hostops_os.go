package machine

import (
	"bufio"
	"bytes"
	"context"
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

// OSHostOps is HostOps on the Machine the process runs on.
type OSHostOps struct {
	// root prefixes every Machine path; "/" outside tests.
	root string
	euid int
}

var _ HostOps = (*OSHostOps)(nil)

// NewOSHostOps returns HostOps for this Machine.
func NewOSHostOps() *OSHostOps {
	return &OSHostOps{root: "/", euid: os.Geteuid()}
}

func (o *OSHostOps) path(p string) string {
	return filepath.Join(o.root, p)
}

// unprivileged marks a permission error as a check the invoking user cannot
// perform.
func unprivileged(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %w", ErrUnprivileged, err)
	}
	return err
}

// LookupUser implements HostOps.
func (o *OSHostOps) LookupUser(name string) (User, bool, error) {
	u, err := user.Lookup(name)
	if errors.As(err, new(user.UnknownUserError)) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, fmt.Errorf("looking up user %s: %w", name, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return User{}, false, fmt.Errorf("user %s: uid %q: %w", name, u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return User{}, false, fmt.Errorf("user %s: gid %q: %w", name, u.Gid, err)
	}
	return User{Name: name, UID: uid, GID: gid, Home: u.HomeDir}, true, nil
}

// SubIDRanges implements HostOps.
func (o *OSHostOps) SubIDRanges(u User) (bool, bool, error) {
	subuid, err := o.hasSubIDRange("/etc/subuid", u)
	if err != nil {
		return false, false, err
	}
	subgid, err := o.hasSubIDRange("/etc/subgid", u)
	if err != nil {
		return false, false, err
	}
	return subuid, subgid, nil
}

// hasSubIDRange reports whether file (subuid(5)/subgid(5): name-or-uid:start:count
// per line) holds a range with a non-zero count for u.
func (o *OSHostOps) hasSubIDRange(file string, u User) (bool, error) {
	data, err := os.ReadFile(o.path(file))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, unprivileged(err)
	}
	uid := strconv.Itoa(u.UID)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Split(strings.TrimSpace(scanner.Text()), ":")
		if len(fields) != 3 || (fields[0] != u.Name && fields[0] != uid) {
			continue
		}
		if count, err := strconv.ParseUint(fields[2], 10, 32); err == nil && count > 0 {
			return true, nil
		}
	}
	return false, scanner.Err()
}

// LingerEnabled implements HostOps.
func (o *OSHostOps) LingerEnabled(u User) (bool, error) {
	return o.exists("/var/lib/systemd/linger/" + u.Name)
}

// UserManagerRunning implements HostOps: the manager's private socket exists
// once user@<uid>.service accepts connections.
func (o *OSHostOps) UserManagerRunning(u User) (bool, error) {
	return o.exists(fmt.Sprintf("/run/user/%d/systemd/private", u.UID))
}

func (o *OSHostOps) exists(p string) (bool, error) {
	info, err := o.Stat(p)
	return info.Exists, err
}

// UserUnitEnabled implements HostOps. Only root and the user can ask the
// user's manager.
func (o *OSHostOps) UserUnitEnabled(ctx context.Context, u User, unit string) (bool, error) {
	runtimeDir := fmt.Sprintf("/run/user/%d", u.UID)
	query := []string{
		"env", "XDG_RUNTIME_DIR=" + runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus",
		"systemctl", "--user", "is-enabled", unit,
	}
	switch o.euid {
	case 0:
		return unitEnabled(ctx, "runuser", append([]string{"-u", u.Name, "--"}, query...)...)
	case u.UID:
		return unitEnabled(ctx, query[0], query[1:]...)
	}
	return false, fmt.Errorf("%w: systemctl --user of %s", ErrUnprivileged, u.Name)
}

// SystemUnitEnabled implements HostOps.
func (o *OSHostOps) SystemUnitEnabled(ctx context.Context, unit string) (bool, error) {
	return unitEnabled(ctx, "systemctl", "is-enabled", unit)
}

// unitEnabled runs a `systemctl is-enabled` command line. It exits non-zero
// for every state but enabled ones and prints the state on stdout; only a
// run without a state is an error. enabled-runtime does not survive a
// reboot, so it counts as not enabled.
func unitEnabled(ctx context.Context, name string, args ...string) (bool, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	state, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if state == "" && err != nil {
		return false, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return state == "enabled", nil
}

// Stat implements HostOps.
func (o *OSHostOps) Stat(p string) (PathInfo, error) {
	info, err := os.Lstat(o.path(p))
	if errors.Is(err, fs.ErrNotExist) {
		return PathInfo{}, nil
	}
	if err != nil {
		return PathInfo{}, unprivileged(err)
	}
	out := PathInfo{Exists: true, Mode: info.Mode()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		out.UID, out.GID = int(st.Uid), int(st.Gid)
	}
	return out, nil
}

// FileContentEquals implements HostOps.
func (o *OSHostOps) FileContentEquals(p string, content []byte) (bool, error) {
	info, err := o.Stat(p)
	if err != nil || !info.Exists || !info.Mode.IsRegular() {
		return false, err
	}
	data, err := os.ReadFile(o.path(p))
	if err != nil {
		return false, unprivileged(err)
	}
	return bytes.Equal(data, content), nil
}

// WorldReadableTree implements HostOps. Symlinks inside the tree are not
// followed; their own mode bits mean nothing.
func (o *OSHostOps) WorldReadableTree(p string) (bool, error) {
	if ok, err := o.ancestorsSearchable(p); err != nil || !ok {
		return false, err
	}
	readable := true
	err := filepath.WalkDir(o.path(p), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !worldReadable(info.Mode()) {
			readable = false
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, unprivileged(err)
	}
	return readable, nil
}

// ancestorsSearchable reports whether every directory above p grants search
// to others.
func (o *OSHostOps) ancestorsSearchable(p string) (bool, error) {
	for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
		info, err := o.Stat(dir)
		if err != nil || !info.Exists || info.Mode.Perm()&0o001 == 0 {
			return false, err
		}
		if dir == filepath.Dir(dir) {
			return true, nil
		}
	}
}

// worldReadable reports whether others can read an entry of mode: a
// directory needs read and search, a symlink's own bits mean nothing.
func worldReadable(mode fs.FileMode) bool {
	switch {
	case mode&fs.ModeSymlink != 0:
		return true
	case mode.IsDir():
		return mode.Perm()&0o005 == 0o005
	}
	return mode.Perm()&0o004 != 0
}
