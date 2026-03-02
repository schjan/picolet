package applier

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// AtomicFileWriter writes files atomically using tmp + rename.
type AtomicFileWriter struct{}

// NewAtomicFileWriter creates a new AtomicFileWriter.
func NewAtomicFileWriter() *AtomicFileWriter {
	return &AtomicFileWriter{}
}

func (w *AtomicFileWriter) WriteFile(path string, content []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming %s to %s: %w", tmp, path, err)
	}
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

// memFileWriter records operations for testing.
type memFileWriter struct {
	written map[string][]byte
	dirs    []string
	removed []string
}

func newMemFileWriter() *memFileWriter {
	return &memFileWriter{written: make(map[string][]byte)}
}

func (w *memFileWriter) WriteFile(path string, content []byte) error {
	w.written[path] = content
	return nil
}

func (w *memFileWriter) MkdirAll(path string) error {
	w.dirs = append(w.dirs, path)
	return nil
}

func (w *memFileWriter) Remove(path string) error {
	w.removed = append(w.removed, path)
	return nil
}
