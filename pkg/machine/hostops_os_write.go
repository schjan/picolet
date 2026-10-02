package machine

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Defaults of the bounded wait for a user manager.
const (
	defaultManagerTimeout = 60 * time.Second
	defaultPollInterval   = 250 * time.Millisecond
)

// runCommand runs a command, its output part of the error.
func runCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(cmd.Args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CreateUser implements HostOps. Never --system: shadow-utils allocates no
// subordinate IDs for system accounts. --create-home explicitly: Debian's
// CREATE_HOME default is off.
func (o *OSHostOps) CreateUser(ctx context.Context, name string) error {
	return runCommand(ctx, "useradd", "--create-home", "--shell", "/usr/sbin/nologin", name)
}

// EnableLinger implements HostOps.
func (o *OSHostOps) EnableLinger(ctx context.Context, u User) error {
	return runCommand(ctx, "loginctl", "enable-linger", u.Name)
}

// WaitUserManager implements HostOps: it polls for the manager's private
// socket, which appears once user@<uid>.service accepts connections.
func (o *OSHostOps) WaitUserManager(ctx context.Context, u User) error {
	deadline := time.NewTimer(o.managerTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(o.pollInterval)
	defer poll.Stop()
	for {
		running, err := o.UserManagerRunning(u)
		if err != nil || running {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("user manager of %s did not start within %s: check systemctl status user@%d.service",
				u.Name, o.managerTimeout, u.UID)
		case <-poll.C:
		}
	}
}

// EnableUserUnit implements HostOps. runuser reaches the user's manager
// through the user's own bus; systemctl --user -M needs systemd-container.
func (o *OSHostOps) EnableUserUnit(ctx context.Context, u User, unit string) error {
	runtimeDir := fmt.Sprintf("/run/user/%d", u.UID)
	return runCommand(ctx, "runuser", "-u", u.Name, "--", "env",
		"XDG_RUNTIME_DIR="+runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path="+runtimeDir+"/bus",
		"systemctl", "--user", "enable", "--now", unit)
}

// EnableSystemUnit implements HostOps.
func (o *OSHostOps) EnableSystemUnit(ctx context.Context, unit string) error {
	return runCommand(ctx, "systemctl", "enable", "--now", unit)
}

// openBelow opens base as an os.Root, so no operation on rel escapes it
// (a Host user owns its home and may have planted symlinks), and checks that
// rel names something below base.
func (o *OSHostOps) openBelow(base, rel string) (*os.Root, error) {
	if !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("%q is not a path below %s", rel, base)
	}
	root, err := os.OpenRoot(o.path(base))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", base, err)
	}
	return root, nil
}

// EnsureDir implements HostOps.
func (o *OSHostOps) EnsureDir(base, rel string, owner Owner, mode fs.FileMode) error {
	root, err := o.openBelow(base, rel)
	if err != nil {
		return err
	}
	defer root.Close()
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	for i := range parts {
		p := filepath.Join(parts[:i+1]...)
		last := i == len(parts)-1
		if err := ensureDirEntry(root, p, last, owner, mode); err != nil {
			return fmt.Errorf("directory %s: %w", filepath.Join(base, p), err)
		}
	}
	return nil
}

// ensureDirEntry makes p a directory: the target (last) with owner and mode,
// a missing parent with owner and 0755. An existing parent may be a symlink
// within the root; the target must be a directory itself.
func ensureDirEntry(root *os.Root, p string, last bool, owner Owner, mode fs.FileMode) error {
	perm, stat := fs.FileMode(0o755), root.Stat
	if last {
		perm, stat = mode, root.Lstat
	}
	info, err := stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Created private, so it is never briefly root-owned and open.
		if err := root.Mkdir(p, 0o700); err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir():
		return errors.New("exists and is not a directory")
	case !last:
		return nil
	}
	// Through a handle on the directory: if the user swaps p after the
	// check, the chown cannot land on another inode (a hard link to a
	// root-owned file).
	dir, err := root.OpenFile(p, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	err = dir.Chown(owner.UID, owner.GID)
	if err == nil {
		err = dir.Chmod(perm)
	}
	return errors.Join(err, dir.Close())
}

// WriteFile implements HostOps: a temporary file in the same directory,
// renamed over rel once complete.
func (o *OSHostOps) WriteFile(base, rel string, content []byte, owner Owner, mode fs.FileMode) error {
	root, err := o.openBelow(base, rel)
	if err != nil {
		return err
	}
	defer root.Close()
	dir, name := filepath.Split(rel)
	tmp := filepath.Join(dir, "."+name+".picolet-"+rand.Text())
	if err := writeTemp(root, tmp, content, owner, mode); err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("writing %s: %w", filepath.Join(base, rel), err)
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("writing %s: %w", filepath.Join(base, rel), err)
	}
	return nil
}

func writeTemp(root *os.Root, tmp string, content []byte, owner Owner, mode fs.FileMode) error {
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if err == nil {
		err = f.Chown(owner.UID, owner.GID)
	}
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// MakeWorldReadable implements HostOps. The walk runs in an os.Root, so a
// directory swapped for a symlink mid-walk cannot lead a chmod out of the
// tree.
func (o *OSHostOps) MakeWorldReadable(p string) error {
	blocked, err := o.unsearchableAncestor(p)
	if err != nil {
		return err
	}
	if blocked != "" {
		return fmt.Errorf("%s is not searchable by other users, so no Host user can reach %s: "+
			"move the checkout (e.g. below /srv) or grant search (chmod o+x %s)", blocked, p, blocked)
	}
	root, err := os.OpenRoot(o.path(p))
	if err != nil {
		return fmt.Errorf("opening %s: %w", p, err)
	}
	defer root.Close()
	if err := fs.WalkDir(root.FS(), ".", grantWorldRead(root)); err != nil {
		return fmt.Errorf("making %s readable by every user: %w", p, err)
	}
	return nil
}

// grantWorldRead adds o+r to every entry it visits, and o+x to directories
// and to files someone may execute (chmod's X); symlinks are skipped.
func grantWorldRead(root *os.Root) fs.WalkDirFunc {
	return func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		want := mode.Perm() | 0o004
		if mode.IsDir() || mode.Perm()&0o111 != 0 {
			want |= 0o001
		}
		if want == mode.Perm() {
			return nil
		}
		return root.Chmod(name, mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)|want)
	}
}
