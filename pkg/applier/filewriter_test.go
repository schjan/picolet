package applier_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/applier"
)

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

func TestAtomicFileWriterWriteFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "unit.container")

	writer := applier.NewAtomicFileWriter()
	require.NoError(t, writer.WriteFile(path, []byte("[Container]\nImage=example\n")))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("[Container]\nImage=example\n"), data)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	matches, err := filepath.Glob(filepath.Join(dir, "unit.container.tmp-*"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}
