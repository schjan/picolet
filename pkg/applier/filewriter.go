package applier

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// AtomicFileWriter writes files atomically using tmp + rename.
type AtomicFileWriter struct{}

// NewAtomicFileWriter creates a new AtomicFileWriter.
func NewAtomicFileWriter() *AtomicFileWriter {
	return &AtomicFileWriter{}
}

// filePerm for quadlet/systemd files; must be world-readable.
// Secrets are managed via the Podman API, not written to disk.
const filePerm = 0o644

func (w *AtomicFileWriter) WriteFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting permissions on %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpName, path, err)
	}
	removeTmp = false
	return nil
}

func (w *AtomicFileWriter) MkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}

func (w *AtomicFileWriter) Remove(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
