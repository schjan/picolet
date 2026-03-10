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
	desired := []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/foo.container", Content: "image=foo", Category: "container", ServiceName: "foo.service"},
		{DestPath: "/etc/containers/systemd/bar.network", Content: "internal=true", Category: "network", ServiceName: "bar-network.service"},
	}
	st := state.NewState()

	cs := Diff(desired, st)

	require.True(t, cs.HasChanges())
	assert.Equal(t, 2, cs.Summary[ActionCreate])
	for _, c := range cs.Changes {
		assert.Equal(t, ActionCreate, c.Action)
		assert.NotEmpty(t, c.NewHash)
		assert.NotEmpty(t, c.ServiceName)
	}
}

func TestDiffNoopWhenUnchanged(t *testing.T) {
	t.Parallel()
	content := "image=foo"
	h := hash(content)

	desired := []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/foo.container", Content: content, Category: "container", ServiceName: "foo.service"},
	}
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: h, Category: "container"},
		},
		ServiceNames: make(map[string]string),
	}

	cs := Diff(desired, st)

	assert.False(t, cs.HasChanges())
	assert.Equal(t, 1, cs.Summary[ActionNoop])
}

func TestDiffUpdateChangedContent(t *testing.T) {
	t.Parallel()

	desired := []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/foo.container", Content: "image=foo:v2", Category: "container"},
	}
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/foo.container": {Hash: "sha256:oldolddead", Category: "container"},
		},
		ServiceNames: make(map[string]string),
	}

	cs := Diff(desired, st)

	require.True(t, cs.HasChanges())
	assert.Equal(t, 1, cs.Summary[ActionUpdate])
}

func TestDiffDeleteRemovedFiles(t *testing.T) {
	t.Parallel()

	desired := []resolver.ResolvedFile{}
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/old.container": {Hash: "sha256:abc", Category: "container"},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/old.container": "old.service",
		},
	}

	cs := Diff(desired, st)

	require.True(t, cs.HasChanges())
	assert.Equal(t, 1, cs.Summary[ActionDelete])
	assert.Equal(t, "container", cs.Changes[0].Category)
	assert.Equal(t, "old.service", cs.Changes[0].ServiceName)
}

func TestDiffMixedOperations(t *testing.T) {
	t.Parallel()

	keepContent := "keep"
	keepHash := hash(keepContent)

	desired := []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/keep.network", Content: keepContent, Category: "network"},
		{DestPath: "/etc/containers/systemd/new.container", Content: "new", Category: "container"},
		{DestPath: "/etc/containers/systemd/update.kube", Content: "updated", Category: "kube"},
	}
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/keep.network":     {Hash: keepHash, Category: "network"},
			"/etc/containers/systemd/update.kube":      {Hash: "sha256:old", Category: "kube"},
			"/etc/containers/systemd/remove.container": {Hash: "sha256:gone", Category: "container"},
		},
		ServiceNames: make(map[string]string),
	}

	cs := Diff(desired, st)

	assert.Equal(t, 1, cs.Summary[ActionNoop])
	assert.Equal(t, 1, cs.Summary[ActionCreate])
	assert.Equal(t, 1, cs.Summary[ActionUpdate])
	assert.Equal(t, 1, cs.Summary[ActionDelete])
}

func TestDiffSecretUpdate(t *testing.T) {
	t.Parallel()

	content := "token=abc"
	sameHash := hash(content)

	desired := []resolver.ResolvedFile{
		{DestPath: "secret:my_secret", Content: content, Category: "secret"},
	}

	// Same content → noop
	stSame := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"secret:my_secret": {Hash: sameHash, Category: "secret"},
		},
		ServiceNames: make(map[string]string),
	}
	csNoop := Diff(desired, stSame)
	assert.Equal(t, 1, csNoop.Summary[ActionNoop], "unchanged secret content should be noop")

	// Different content hash → update
	stOld := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"secret:my_secret": {Hash: "sha256:old", Category: "secret"},
		},
		ServiceNames: make(map[string]string),
	}
	csUpdate := Diff(desired, stOld)
	assert.Equal(t, 1, csUpdate.Summary[ActionUpdate], "changed secret content should produce update")
}

func TestVolumeFileFromPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		destPath    string
		wantVolume  string
		wantRelPath string
		wantOk      bool
	}{
		{
			name:        "valid volumefile path",
			destPath:    "volumefile:data:app.conf",
			wantVolume:  "data",
			wantRelPath: "app.conf",
			wantOk:      true,
		},
		{
			name:        "rel path with colons preserved",
			destPath:    "volumefile:vol:sub:dir:file.conf",
			wantVolume:  "vol",
			wantRelPath: "sub:dir:file.conf",
			wantOk:      true,
		},
		{
			name:     "non-volumefile path",
			destPath: "secret:foo",
			wantOk:   false,
		},
		{
			name:     "empty string",
			destPath: "",
			wantOk:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			volume, relPath, ok := VolumeFileFromPath(tt.destPath)
			assert.Equal(t, tt.wantOk, ok)
			if tt.wantOk {
				assert.Equal(t, tt.wantVolume, volume)
				assert.Equal(t, tt.wantRelPath, relPath)
			}
		})
	}
}

func TestDiffServiceNamePropagated(t *testing.T) {
	t.Parallel()

	desired := []resolver.ResolvedFile{
		{DestPath: "/etc/containers/systemd/app.container", Content: "content", Category: "container", ServiceName: "app.service"},
	}
	st := state.NewState()

	cs := Diff(desired, st)

	require.Equal(t, 1, cs.Summary[ActionCreate])
	assert.Equal(t, "app.service", cs.Changes[0].ServiceName)
}
