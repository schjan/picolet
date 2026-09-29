package resolver

import (
	"fmt"
	"os"
)

// DirSecretReader reads host-only secrets from dir, confined to it: a secret
// name or symlink cannot reach files outside. The directory is opened on the
// first read, so a resolve that reads no host secret does not need it to
// exist. Call release once the resolve is done.
func DirSecretReader(dir string) (read SecretReader, release func()) {
	var root *os.Root
	read = func(path string) (string, error) {
		if root == nil {
			r, err := os.OpenRoot(dir)
			if err != nil {
				return "", fmt.Errorf("opening secrets dir: %w", err)
			}
			root = r
		}
		data, err := root.ReadFile(path)
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
