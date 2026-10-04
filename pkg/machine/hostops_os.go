package machine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// OSHostOps is HostOps on the Machine the process runs on.
type OSHostOps struct {
	// root prefixes every Machine path; "/" outside tests.
	root string
	euid int
	// managerTimeout bounds WaitUserManager, which checks every pollInterval.
	managerTimeout time.Duration
	pollInterval   time.Duration
}

var _ HostOps = (*OSHostOps)(nil)

// NewOSHostOps returns HostOps for this Machine.
func NewOSHostOps() *OSHostOps {
	return &OSHostOps{root: "/", euid: os.Geteuid(), managerTimeout: defaultManagerTimeout, pollInterval: defaultPollInterval}
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
// per line) holds a usable range for u: a valid start, a non-zero count, and
// an end inside the 32-bit ID space.
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
		if len(fields) == 3 && (fields[0] == u.Name || fields[0] == uid) && validSubIDRange(fields[1], fields[2]) {
			return true, nil
		}
	}
	return false, scanner.Err()
}

func validSubIDRange(start, count string) bool {
	first, err := strconv.ParseUint(start, 10, 32)
	if err != nil {
		return false
	}
	n, err := strconv.ParseUint(count, 10, 32)
	return err == nil && n > 0 && first+n-1 <= math.MaxUint32
}

// LingerEnabled implements HostOps.
func (o *OSHostOps) LingerEnabled(u User) (bool, error) {
	return o.exists("/var/lib/systemd/linger/" + u.Name)
}

// UserManagerRunning implements HostOps: the manager's private socket exists
// once user@<uid>.service accepts connections.
func (o *OSHostOps) UserManagerRunning(u User) (bool, error) {
	return o.exists(u.runtimeDir() + "/systemd/private")
}

func (o *OSHostOps) exists(p string) (bool, error) {
	info, err := o.Stat(p)
	return info.Exists, err
}

// UserUnitState implements HostOps. Only root and the user can ask the
// user's manager. Root asks as the user by switching credentials in the
// child directly: runuser/su would open a PAM session, whose hooks
// (pam_systemd, pam_mkhomedir) may write to the Machine.
func (o *OSHostOps) UserUnitState(ctx context.Context, u User, unit string) (UnitState, error) {
	cmd := unitStateCommand(ctx, "--user", unit)
	runtimeDir := u.runtimeDir()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + u.Home, "USER=" + u.Name, "LOGNAME=" + u.Name,
		"XDG_RUNTIME_DIR=" + runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus",
	}
	switch o.euid {
	case 0:
		cred, err := credential(u)
		if err != nil {
			return UnitState{}, err
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	case u.UID:
	default:
		return UnitState{}, fmt.Errorf("%w: systemctl --user of %s", ErrUnprivileged, u.Name)
	}
	return unitState(cmd)
}

// credential is u's uid and primary gid with no supplementary groups, so the
// child keeps none of root's.
func credential(u User) (*syscall.Credential, error) {
	if u.UID < 0 || uint64(u.UID) > math.MaxUint32 || u.GID < 0 || uint64(u.GID) > math.MaxUint32 {
		return nil, fmt.Errorf("user %s: uid %d / gid %d out of range", u.Name, u.UID, u.GID)
	}
	return &syscall.Credential{Uid: uint32(u.UID), Gid: uint32(u.GID), Groups: []uint32{}}, nil
}

// SystemUnitState implements HostOps.
func (o *OSHostOps) SystemUnitState(ctx context.Context, unit string) (UnitState, error) {
	return unitState(unitStateCommand(ctx, "--system", unit))
}

func unitStateCommand(ctx context.Context, manager, unit string) *exec.Cmd {
	return exec.CommandContext(ctx, "systemctl", manager, "show", "--property=UnitFileState", "--property=ActiveState", unit)
}

// unitState runs a unitStateCommand and parses its KEY=value lines. A unit
// systemd does not know shows as neither enabled nor active.
func unitState(cmd *exec.Cmd) (UnitState, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return UnitState{}, fmt.Errorf("%s: %w: %s", strings.Join(cmd.Args, " "), err, strings.TrimSpace(stderr.String()))
	}
	var state UnitState
	for line := range strings.Lines(string(out)) {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "UnitFileState":
			state.Enabled = value == "enabled"
		case "ActiveState":
			state.Active = value == "active"
		}
	}
	return state, nil
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
	if blocked, err := o.unsearchableAncestor(p); err != nil || blocked != "" {
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

// unsearchableAncestor returns the first directory above p, from p upwards,
// that is missing or denies search to others; "" when every one grants it.
func (o *OSHostOps) unsearchableAncestor(p string) (string, error) {
	for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
		info, err := o.Stat(dir)
		if err != nil {
			return "", err
		}
		if !info.Exists || info.Mode.Perm()&0o001 == 0 {
			return dir, nil
		}
		if dir == filepath.Dir(dir) {
			return "", nil
		}
	}
}

// worldReadable reports whether others can read an entry of mode as chmod
// o+rX would leave it; a symlink's own bits mean nothing.
func worldReadable(mode fs.FileMode) bool {
	return mode&fs.ModeSymlink != 0 || worldReadableMode(mode) == mode
}

// worldReadableMode is mode after chmod o+rX: read for others, and search
// or execute for others on a directory or a file someone may execute.
func worldReadableMode(mode fs.FileMode) fs.FileMode {
	add := fs.FileMode(0o004)
	if mode.IsDir() || mode.Perm()&0o111 != 0 {
		add |= 0o001
	}
	return mode | add
}
