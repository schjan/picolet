package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissing(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	st, err := store.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.AppliedSHA != "" {
		t.Errorf("AppliedSHA = %q, want empty", st.AppliedSHA)
	}
	if st.ManagedFiles == nil {
		t.Fatal("ManagedFiles should not be nil")
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)

	now := time.Now().Truncate(time.Second)
	want := &State{
		AppliedSHA: "abc123",
		AppliedAt:  now,
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/foo.container": "sha256:deadbeef",
		},
	}

	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.AppliedSHA != want.AppliedSHA {
		t.Errorf("AppliedSHA = %q, want %q", got.AppliedSHA, want.AppliedSHA)
	}
	if !got.AppliedAt.Equal(want.AppliedAt) {
		t.Errorf("AppliedAt = %v, want %v", got.AppliedAt, want.AppliedAt)
	}
	if len(got.ManagedFiles) != 1 {
		t.Fatalf("ManagedFiles count = %d, want 1", len(got.ManagedFiles))
	}
	if got.ManagedFiles["/etc/containers/systemd/foo.container"] != "sha256:deadbeef" {
		t.Errorf("unexpected ManagedFiles content: %v", got.ManagedFiles)
	}
}

func TestSaveCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subdir", "state.json")
	store := NewStore(path)

	st := &State{AppliedSHA: "test", ManagedFiles: make(map[string]string)}
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AppliedSHA != "test" {
		t.Errorf("AppliedSHA = %q, want test", got.AppliedSHA)
	}
}

func TestSaveRoundtripWithFailedSHA(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)

	st := &State{
		AppliedSHA:   "abc",
		FailedSHA:    "def",
		ManagedFiles: make(map[string]string),
	}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.FailedSHA != "def" {
		t.Errorf("FailedSHA = %q, want def", got.FailedSHA)
	}
}
