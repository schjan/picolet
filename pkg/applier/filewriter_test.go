package applier_test

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
