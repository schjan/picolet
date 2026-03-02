package rollback

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/schjan/picolet/pkg/reconciler"
)

type mockWriter struct {
	written map[string][]byte
	removed []string
}

func newMockWriter() *mockWriter {
	return &mockWriter{written: make(map[string][]byte)}
}

func (w *mockWriter) WriteFile(path string, content []byte) error {
	w.written[path] = content
	return nil
}

func (w *mockWriter) MkdirAll(string) error { return nil }

func (w *mockWriter) Remove(path string) error {
	w.removed = append(w.removed, path)
	return nil
}

type mockSystemd struct {
	reloads int
}

func (m *mockSystemd) DaemonReload(context.Context) error {
	m.reloads++
	return nil
}

func (m *mockSystemd) StartUnit(context.Context, string) error   { return nil }
func (m *mockSystemd) RestartUnit(context.Context, string) error { return nil }
func (m *mockSystemd) GetUnitState(context.Context, string) (string, error) {
	return "active", nil
}
func (m *mockSystemd) IsActive(context.Context, string) (bool, error) { return true, nil }

//nolint:cyclop // integration test: snapshot + restore
func TestCreateAndRestore(t *testing.T) {
	w := newMockWriter()
	sys := &mockSystemd{}
	mgr := New(w, sys)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/new.container", Action: reconciler.ActionCreate, Category: "container"},
			{DestPath: "/etc/containers/systemd/old.container", Action: reconciler.ActionUpdate, Category: "container"},
			{DestPath: "secret:my_secret", Action: reconciler.ActionCreate, Category: "secret"},
		},
	}

	// Mock disk reader: old.container exists, new.container does not
	diskReader := func(path string) ([]byte, error) {
		if path == "/etc/containers/systemd/old.container" {
			return []byte("original-content"), nil
		}
		return nil, fmt.Errorf("not found: %s", path)
	}

	snap, err := mgr.Create(cs, diskReader)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Secret should be skipped
	if _, ok := snap.Files["secret:my_secret"]; ok {
		t.Error("secret should be skipped in snapshot")
	}

	// new.container → nil (didn't exist)
	if content, ok := snap.Files["/etc/containers/systemd/new.container"]; !ok || content != nil {
		t.Error("new.container should be nil in snapshot")
	}

	// old.container → original content
	if content, ok := snap.Files["/etc/containers/systemd/old.container"]; !ok || string(content) != "original-content" {
		t.Error("old.container should have original content in snapshot")
	}

	// Now restore
	if err := mgr.Restore(context.Background(), snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// new.container should be removed
	if len(w.removed) != 1 || w.removed[0] != "/etc/containers/systemd/new.container" {
		t.Errorf("removed = %v, want [/etc/containers/systemd/new.container]", w.removed)
	}

	// old.container should be restored
	if string(w.written["/etc/containers/systemd/old.container"]) != "original-content" {
		t.Error("old.container not restored correctly")
	}

	// daemon-reload should be called
	if sys.reloads != 1 {
		t.Errorf("reloads = %d, want 1", sys.reloads)
	}
}

func TestSnapshotWithRealFilesystem(t *testing.T) {
	dir := t.TempDir()
	existingPath := filepath.Join(dir, "existing.conf")
	if err := os.WriteFile(existingPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := newMockWriter()
	sys := &mockSystemd{}
	mgr := New(w, sys)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: existingPath, Action: reconciler.ActionUpdate, Category: "container"},
			{DestPath: filepath.Join(dir, "new.conf"), Action: reconciler.ActionCreate, Category: "network"},
		},
	}

	snap, err := mgr.Create(cs, os.ReadFile)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if string(snap.Files[existingPath]) != "existing" {
		t.Errorf("snapshot of existing file = %q, want existing", snap.Files[existingPath])
	}
	if snap.Files[filepath.Join(dir, "new.conf")] != nil {
		t.Error("new file should be nil in snapshot")
	}
}

func TestCreateSkipsNoop(t *testing.T) {
	w := newMockWriter()
	sys := &mockSystemd{}
	mgr := New(w, sys)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/noop.container", Action: reconciler.ActionNoop, Category: "container"},
		},
	}

	snap, err := mgr.Create(cs, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(snap.Files) != 0 {
		t.Errorf("snapshot should be empty for noop, got %d files", len(snap.Files))
	}
}
