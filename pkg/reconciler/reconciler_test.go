package reconciler

import (
	"errors"
	"testing"

	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
)

func TestDiffCreateNewFiles(t *testing.T) {
	r := New()
	desired := []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/foo.container", Content: "image=foo", Category: "container"},
		{DestPath: "/etc/containers/systemd/bar.network", Content: "internal=true", Category: "network"},
	}
	st := &state.State{ManagedFiles: make(map[string]string)}

	cs := r.Diff(desired, st, nil, nil)

	if !cs.HasChanges() {
		t.Fatal("expected changes")
	}
	if cs.Summary[ActionCreate] != 2 {
		t.Errorf("create count = %d, want 2", cs.Summary[ActionCreate])
	}
	for _, c := range cs.Changes {
		if c.Action != ActionCreate {
			t.Errorf("change %s: action = %s, want create", c.DestPath, c.Action)
		}
		if c.NewHash == "" {
			t.Errorf("change %s: NewHash should not be empty", c.DestPath)
		}
	}
}

func TestDiffNoopWhenUnchanged(t *testing.T) {
	r := New()
	content := "image=foo"
	h := hash(content)

	desired := []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/foo.container", Content: content, Category: "container"},
	}
	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/foo.container": h,
		},
	}

	cs := r.Diff(desired, st, nil, nil)

	if cs.HasChanges() {
		t.Fatal("expected no changes")
	}
	if cs.Summary[ActionNoop] != 1 {
		t.Errorf("noop count = %d, want 1", cs.Summary[ActionNoop])
	}
}

func TestDiffUpdateChangedContent(t *testing.T) {
	r := New()

	desired := []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/foo.container", Content: "image=foo:v2", Category: "container"},
	}
	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/foo.container": "sha256:oldolddead",
		},
	}

	cs := r.Diff(desired, st, nil, nil)

	if !cs.HasChanges() {
		t.Fatal("expected changes")
	}
	if cs.Summary[ActionUpdate] != 1 {
		t.Errorf("update count = %d, want 1", cs.Summary[ActionUpdate])
	}
}

func TestDiffDeleteRemovedFiles(t *testing.T) {
	r := New()

	desired := []resolver.ResolvedFile{} // empty desired state
	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/old.container": "sha256:abc",
		},
	}

	cs := r.Diff(desired, st, nil, nil)

	if !cs.HasChanges() {
		t.Fatal("expected changes")
	}
	if cs.Summary[ActionDelete] != 1 {
		t.Errorf("delete count = %d, want 1", cs.Summary[ActionDelete])
	}
	if cs.Changes[0].Category != "container" {
		t.Errorf("category = %q, want container", cs.Changes[0].Category)
	}
}

func TestDiffMixedOperations(t *testing.T) {
	r := New()

	keepContent := "keep"
	keepHash := hash(keepContent)

	desired := []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/keep.network", Content: keepContent, Category: "network"},
		{DestPath: "/etc/containers/systemd/new.container", Content: "new", Category: "container"},
		{DestPath: "/etc/containers/systemd/update.kube", Content: "updated", Category: "kube"},
	}
	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/keep.network":     keepHash,
			"/etc/containers/systemd/update.kube":      "sha256:old",
			"/etc/containers/systemd/remove.container": "sha256:gone",
		},
	}

	cs := r.Diff(desired, st, nil, nil)

	if cs.Summary[ActionNoop] != 1 {
		t.Errorf("noop = %d, want 1", cs.Summary[ActionNoop])
	}
	if cs.Summary[ActionCreate] != 1 {
		t.Errorf("create = %d, want 1", cs.Summary[ActionCreate])
	}
	if cs.Summary[ActionUpdate] != 1 {
		t.Errorf("update = %d, want 1", cs.Summary[ActionUpdate])
	}
	if cs.Summary[ActionDelete] != 1 {
		t.Errorf("delete = %d, want 1", cs.Summary[ActionDelete])
	}
}

func TestDiffSecretSkipIfExists(t *testing.T) {
	r := New()

	desired := []resolver.ResolvedFile{
		{DestPath: "secret:my_secret", Content: "token=abc", Category: "secret"},
	}
	st := &state.State{
		ManagedFiles: map[string]string{
			"secret:my_secret": "sha256:old",
		},
	}

	// Secret exists in Podman → noop
	checker := func(name string) (bool, error) {
		if name == "my_secret" {
			return true, nil
		}
		return false, nil
	}

	cs := r.Diff(desired, st, nil, checker)
	if cs.Summary[ActionNoop] != 1 {
		t.Errorf("noop = %d, want 1 (skip_if_exists)", cs.Summary[ActionNoop])
	}

	// Secret does NOT exist in Podman → update (hash differs)
	noChecker := func(name string) (bool, error) {
		return false, nil
	}
	cs2 := r.Diff(desired, st, nil, noChecker)
	if cs2.Summary[ActionUpdate] != 1 {
		t.Errorf("update = %d, want 1 (secret missing)", cs2.Summary[ActionUpdate])
	}
}

func TestDiffSecretCheckerError(t *testing.T) {
	r := New()

	desired := []resolver.ResolvedFile{
		{DestPath: "secret:err_secret", Content: "data", Category: "secret"},
	}
	st := &state.State{
		ManagedFiles: map[string]string{
			"secret:err_secret": "sha256:old",
		},
	}

	errChecker := func(name string) (bool, error) {
		return false, errors.New("connection refused")
	}

	// On error, fall through to normal diff (update because hash differs)
	cs := r.Diff(desired, st, nil, errChecker)
	if cs.Summary[ActionUpdate] != 1 {
		t.Errorf("update = %d, want 1 (fallback on error)", cs.Summary[ActionUpdate])
	}
}

func TestCategoryFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"secret:foo", "secret"},
		{"/etc/containers/systemd/foo.container", "container"},
		{"/etc/containers/systemd/foo.network", "network"},
		{"/etc/containers/systemd/foo.volume", "volume"},
		{"/etc/containers/systemd/foo.kube", "kube"},
		{"/etc/systemd/system/foo.socket", "systemd"},
		{"/var/lib/picolet/manifests/app/deploy.yml", "manifest"},
		{"/some/other/path", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := categoryFromPath(tt.path)
			if got != tt.want {
				t.Errorf("categoryFromPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
