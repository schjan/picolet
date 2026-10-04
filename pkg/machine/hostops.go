package machine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
)

// ErrUnprivileged reports that a read-side check cannot be performed by the
// invoking user: another user's 0700 home, `systemctl --user` for another
// user. The plan shows such a step as unknown; as root every check is
// definitive.
var ErrUnprivileged = errors.New("cannot be checked without root")

// User is a Linux user on the Machine.
type User struct {
	Name string
	UID  int
	GID  int
	Home string
}

// Owner is the uid and gid a written path belongs to.
type Owner struct {
	UID int
	GID int
}

// Owner is the user as the owner of a path: its uid and primary gid.
func (u User) Owner() Owner {
	return Owner{UID: u.UID, GID: u.GID}
}

// runtimeDir is the user's XDG_RUNTIME_DIR, which logind creates for a
// lingering user: the home of its manager's sockets and its Podman socket.
func (u User) runtimeDir() string {
	return fmt.Sprintf("/run/user/%d", u.UID)
}

// UnitState is what bootstrap needs to know of a systemd unit.
type UnitState struct {
	// Enabled: the unit starts at boot (enabled-runtime does not survive a
	// reboot and does not count).
	Enabled bool
	// Active: the unit is running now; for a socket, listening.
	Active bool
}

// PathInfo describes a path on the Machine. Mode carries the type bits
// (fs.ModeDir, fs.ModeSymlink) and the permission bits of the path itself;
// symlinks are not followed.
type PathInfo struct {
	Exists bool
	UID    int
	GID    int
	Mode   fs.FileMode
}

// Command is a program run on the Machine.
type Command struct {
	// Args are the program and its arguments.
	Args []string
	// Env are KEY=value pairs added to the environment.
	Env []string
	// Dir is the working directory.
	Dir string
}

// HostOps is the Machine seen by bootstrap machine. The read side answers a
// step's check; every read method returns an error wrapping ErrUnprivileged
// when the invoking user cannot answer the question. The write side brings a
// step's end state about and needs root.
type HostOps interface {
	// LookupUser returns the Linux user named name; found is false when no
	// such user exists.
	LookupUser(name string) (user User, found bool, err error)
	// SubIDRanges reports whether /etc/subuid and /etc/subgid each hold a
	// non-empty range for the user.
	SubIDRanges(user User) (subuid, subgid bool, err error)
	// LingerEnabled reports whether lingering is enabled for the user.
	LingerEnabled(user User) (bool, error)
	// UserManagerRunning reports whether the user's systemd manager
	// (user@<uid>.service) is up and accepting connections.
	UserManagerRunning(user User) (bool, error)
	// UserUnitState reports the state of unit in the user's manager.
	UserUnitState(ctx context.Context, user User, unit string) (UnitState, error)
	// SystemUnitState reports the state of unit in the system manager.
	SystemUnitState(ctx context.Context, unit string) (UnitState, error)
	// Stat describes path without following a final symlink.
	Stat(path string) (PathInfo, error)
	// FileContentEquals reports whether path is a regular file holding
	// exactly content; false when it does not exist.
	FileContentEquals(path string, content []byte) (bool, error)
	// WorldReadableTree reports whether every user on the Machine can read
	// the tree at path: every entry grants read to others, every directory
	// (the tree's and the path's ancestors) grants search to others. False
	// when path does not exist.
	WorldReadableTree(path string) (bool, error)

	// CreateUser creates the Linux user name with a home directory, no
	// password and no login shell; a regular account, so shadow-utils
	// allocates its subordinate ID ranges.
	CreateUser(ctx context.Context, name string) error
	// EnableLinger enables lingering for the user, which starts the user's
	// systemd manager asynchronously.
	EnableLinger(ctx context.Context, user User) error
	// WaitUserManager waits, bounded, until the user's systemd manager
	// accepts connections.
	WaitUserManager(ctx context.Context, user User) error
	// EnableUserUnit enables and starts unit in the user's manager.
	EnableUserUnit(ctx context.Context, user User, unit string) error
	// EnableSystemUnit enables and starts unit in the system manager.
	EnableSystemUnit(ctx context.Context, unit string) error
	// EnsureDir makes rel, below the directory base, a directory with owner
	// and permission mode. Missing directories between base and rel are
	// created with owner and mode 0755; existing ones are left alone.
	// Nothing outside base is touched: a symlink leading out of it is an
	// error.
	EnsureDir(base, rel string, owner Owner, mode fs.FileMode) error
	// WriteFile atomically replaces rel, below the directory base, with a
	// regular file holding content, with owner and permission mode. Its
	// directory must exist; nothing outside base is touched. A directory at
	// rel is an error, never removed.
	WriteFile(base, rel string, content []byte, owner Owner, mode fs.FileMode) error
	// SetOwnerMode gives the regular file rel, below the directory base,
	// owner and permission mode in place, its content untouched. A symlink
	// at rel is not followed; nothing outside base is touched.
	SetOwnerMode(base, rel string, owner Owner, mode fs.FileMode) error
	// MakeWorldReadable grants every user on the Machine read access to the
	// tree at path, like chmod -R o+rX; symlinks are not followed. It fails,
	// changing nothing, when a directory above path denies search to others.
	MakeWorldReadable(path string) error
	// RunAsUser runs cmd as the user, through runuser: a session of the
	// user's own, so rootless Podman runs as the user, with the user's
	// storage and runtime directory. Its output is part of the error.
	RunAsUser(ctx context.Context, user User, cmd Command) error
	// RunAsRoot runs cmd as root. Its output is part of the error.
	RunAsRoot(ctx context.Context, cmd Command) error
	// RestartUserUnit restarts unit in the user's manager.
	RestartUserUnit(ctx context.Context, user User, unit string) error
	// RestartSystemUnit restarts unit in the system manager.
	RestartSystemUnit(ctx context.Context, unit string) error
	// WaitHealthy waits, bounded, until the Agent at addr (host:port)
	// answers its /health with 200.
	WaitHealthy(ctx context.Context, addr string) error
}
