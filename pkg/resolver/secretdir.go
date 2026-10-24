package resolver

import (
	"fmt"
	"io"
	"io/fs"
	"os"
)

// SecretGuard vets a host secret before it is read: it returns an error for
// a file that must not be deployed. It gets the info of the file actually
// opened, so it can compare with os.SameFile and see through symlinks, hard
// links and differently spelled paths.
type SecretGuard func(info fs.FileInfo) error

// DirSecretReader reads host-only secrets from dir, confined to it: a secret
// name or symlink cannot reach files outside. The directory is opened on the
// first read, so a resolve that reads no host secret does not need it to
// exist. A non-nil guard is asked about every file before it is read (the
// Agent passes agentcfg's Config.RefuseCredential). Call release once the
// resolve is done.
func DirSecretReader(dir string, guard SecretGuard) (read SecretReader, release func()) {
	var root *os.Root
	read = func(path string) (string, error) {
		if root == nil {
			r, err := os.OpenRoot(dir)
			if err != nil {
				return "", fmt.Errorf("opening secrets dir: %w", err)
			}
			root = r
		}
		data, err := readGuarded(root, path, guard)
		if err != nil {
			return "", fmt.Errorf("reading secret %q: %w", path, err)
		}
		return string(data), nil
	}
	release = func() {
		if root != nil {
			_ = root.Close()
		}
	}
	return read, release
}

func readGuarded(root *os.Root, path string, guard SecretGuard) ([]byte, error) {
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if guard != nil {
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if err := guard(info); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}
