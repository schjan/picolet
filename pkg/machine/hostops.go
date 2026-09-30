package machine

import (
	"context"
	"errors"
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

// PathInfo describes a path on the Machine. Mode carries the type bits
// (fs.ModeDir, fs.ModeSymlink) and the permission bits of the path itself;
// symlinks are not followed.
type PathInfo struct {
	Exists bool
	UID    int
	GID    int
	Mode   fs.FileMode
}

// HostOps is the Machine seen by bootstrap machine. Only the read side exists
// so far: --plan runs checks and never writes. Every method returns an error
// wrapping ErrUnprivileged when the invoking user cannot answer the question.
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
	// UserUnitEnabled reports whether unit is enabled in the user's manager.
	UserUnitEnabled(ctx context.Context, user User, unit string) (bool, error)
	// SystemUnitEnabled reports whether unit is enabled in the system manager.
	SystemUnitEnabled(ctx context.Context, unit string) (bool, error)
	// Stat describes path without following a final symlink.
	Stat(path string) (PathInfo, error)
	// FileContentEquals reports whether path is a regular file holding
	// exactly content; false when it does not exist.
	FileContentEquals(path string, content []byte) (bool, error)
	// WorldReadableTree reports whether every user on the Machine can read
	// the tree at path: every entry grants read to others, every directory
	// (the tree's and the path's ancestors) grants search to others.
	WorldReadableTree(path string) (bool, error)
}
