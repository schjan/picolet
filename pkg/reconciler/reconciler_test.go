package reconciler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
)

func TestDiffCreateNewFiles(t *testing.T) {
	t.Parallel()
	r := New()
	desired := []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/foo.container", Content: "image=foo", Category: "container"},
		{DestPath: "/etc/containers/systemd/bar.network", Content: "internal=true", Category: "network"},
	}
	st := &state.State{ManagedFiles: make(map[string]string)}

	cs := r.Diff(desired, st)

	require.True(t, cs.HasChanges())
	assert.Equal(t, 2, cs.Summary[ActionCreate])
	for _, c := range cs.Changes {
		assert.Equal(t, ActionCreate, c.Action)
		assert.NotEmpty(t, c.NewHash)
	}
}

func TestDiffNoopWhenUnchanged(t *testing.T) {
	t.Parallel()
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

	cs := r.Diff(desired, st)

	assert.False(t, cs.HasChanges())
	assert.Equal(t, 1, cs.Summary[ActionNoop])
}

func TestDiffUpdateChangedContent(t *testing.T) {
	t.Parallel()
	r := New()

	desired := []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/foo.container", Content: "image=foo:v2", Category: "container"},
	}
	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/foo.container": "sha256:oldolddead",
		},
	}

	cs := r.Diff(desired, st)

	require.True(t, cs.HasChanges())
	assert.Equal(t, 1, cs.Summary[ActionUpdate])
}

func TestDiffDeleteRemovedFiles(t *testing.T) {
	t.Parallel()
	r := New()

	desired := []resolver.ResolvedFile{}
	st := &state.State{
		ManagedFiles: map[string]string{
			"/etc/containers/systemd/old.container": "sha256:abc",
		},
	}

	cs := r.Diff(desired, st)

	require.True(t, cs.HasChanges())
	assert.Equal(t, 1, cs.Summary[ActionDelete])
	assert.Equal(t, "container", cs.Changes[0].Category)
}

func TestDiffMixedOperations(t *testing.T) {
	t.Parallel()
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

	cs := r.Diff(desired, st)

	assert.Equal(t, 1, cs.Summary[ActionNoop])
	assert.Equal(t, 1, cs.Summary[ActionCreate])
	assert.Equal(t, 1, cs.Summary[ActionUpdate])
	assert.Equal(t, 1, cs.Summary[ActionDelete])
}

func TestDiffSecretUpdate(t *testing.T) {
	t.Parallel()
	r := New()

	content := "token=abc"
	sameHash := hash(content)

	desired := []resolver.ResolvedFile{
		{DestPath: "secret:my_secret", Content: content, Category: "secret"},
	}

	// Same content → noop
	stSame := &state.State{
		ManagedFiles: map[string]string{
			"secret:my_secret": sameHash,
		},
	}
	csNoop := r.Diff(desired, stSame)
	assert.Equal(t, 1, csNoop.Summary[ActionNoop], "unchanged secret content should be noop")

	// Different content hash → update
	stOld := &state.State{
		ManagedFiles: map[string]string{
			"secret:my_secret": "sha256:old",
		},
	}
	csUpdate := r.Diff(desired, stOld)
	assert.Equal(t, 1, csUpdate.Summary[ActionUpdate], "changed secret content should produce update")
}

func TestCategoryFromPath(t *testing.T) {
	t.Parallel()
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
		{"/home/runner/.local/share/picolet/manifests/app/deploy.yml", "manifest"},
		{"/some/other/path", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, categoryFromPath(tt.path))
		})
	}
}
