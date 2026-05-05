package resolver

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandServiceBundlesHappyPath(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/containers/web.container.tmpl":     &fstest.MapFile{Data: []byte("container")},
		"services/web/volumes/data.volume":               &fstest.MapFile{Data: []byte("volume")},
		"services/web/networks/internal.network":         &fstest.MapFile{Data: []byte("network")},
		"services/web/kube/app.kube.tmpl":                &fstest.MapFile{Data: []byte("kube")},
		"services/web/systemd/http.socket":               &fstest.MapFile{Data: []byte("socket")},
		"services/web/secrets/config.yml.tmpl":           &fstest.MapFile{Data: []byte("secret")},
		"services/web/picolet.yml":                       &fstest.MapFile{Data: []byte("secret_hooks: []\n")},
		"services/web/manifests/app/deployment.yml.tmpl": &fstest.MapFile{Data: []byte("manifest")},
		"services/web/manifests/app/configs/app.conf":    &fstest.MapFile{Data: []byte("config")},
	}

	expanded, err := expandServiceBundles(fsys, []string{"web"})
	require.NoError(t, err)

	assert.Equal(t, []string{"services/web/networks/internal.network"}, expanded.Networks)
	assert.Equal(t, []string{"services/web/systemd/http.socket"}, expanded.Systemd)
	assert.Equal(t, []string{"services/web/volumes/data.volume"}, expanded.Volumes)
	assert.Equal(t, []string{"services/web/containers/web.container.tmpl"}, expanded.Containers)
	assert.Equal(t, []string{"services/web/kube/app.kube.tmpl"}, expanded.Kube)
	assert.Equal(t, []string{"services/web/secrets/config.yml.tmpl"}, expanded.Secrets)
	assert.Equal(t, []hookRef{{Service: "web", SrcPath: "services/web/picolet.yml"}}, expanded.Hooks)
	assert.Equal(t, []manifestRef{
		{
			SrcPath:     "services/web/manifests/app/configs/app.conf",
			LogicalPath: "manifests/app/configs/app.conf",
		},
		{
			SrcPath:     "services/web/manifests/app/deployment.yml.tmpl",
			LogicalPath: "manifests/app/deployment.yml.tmpl",
		},
	}, expanded.Manifests)
}

func TestExpandServiceBundlesMetadataOnlyIsEmpty(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/picolet.yml": &fstest.MapFile{Data: []byte("secret_hooks: []\n")},
	}

	_, err := expandServiceBundles(fsys, []string{"web"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "services/web: empty service bundle")
}

func TestExpandServiceBundlesRejectsMetadataSymlink(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/picolet.yml":              &fstest.MapFile{Mode: fs.ModeSymlink},
		"services/web/containers/web.container": &fstest.MapFile{Data: []byte("container")},
	}

	_, err := expandServiceBundles(fsys, []string{"web"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "services/web/picolet.yml: expected regular file")
}

func TestExpandServiceBundlesMissingBundle(t *testing.T) {
	t.Parallel()

	_, err := expandServiceBundles(fstest.MapFS{}, []string{"ghost"})
	require.ErrorContains(t, err, "missing service bundle")
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestExpandServiceBundlesBundleRootNotDirectory(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web": &fstest.MapFile{Data: []byte("not a dir")},
	}

	_, err := expandServiceBundles(fsys, []string{"web"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "services/web: expected directory")
}

func TestExpandServiceBundlesEmptyBundle(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web": &fstest.MapFile{Mode: fs.ModeDir},
	}

	_, err := expandServiceBundles(fsys, []string{"web"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "services/web: empty service bundle")
}

func TestExpandServiceBundlesEmptySubdirIsFine(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/containers":                &fstest.MapFile{Mode: fs.ModeDir},
		"services/web/networks/internal.network": &fstest.MapFile{Data: []byte("network")},
	}

	expanded, err := expandServiceBundles(fsys, []string{"web"})
	require.NoError(t, err)
	assert.Equal(t, []string{"services/web/networks/internal.network"}, expanded.Networks)
	assert.Empty(t, expanded.Containers)
}

func TestExpandServiceBundlesUnknownRootEntry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fsys fstest.MapFS
	}{
		{
			name: "unknown directory",
			fsys: fstest.MapFS{
				"services/web/random":                   &fstest.MapFile{Mode: fs.ModeDir},
				"services/web/containers/web.container": &fstest.MapFile{Data: []byte("container")},
			},
		},
		{
			name: "unknown file",
			fsys: fstest.MapFS{
				"services/web/README.md":                &fstest.MapFile{Data: []byte("docs")},
				"services/web/containers/web.container": &fstest.MapFile{Data: []byte("container")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := expandServiceBundles(tt.fsys, []string{"web"})
			require.ErrorContains(t, err, "unknown entry")
			// With only junk at the root, the "empty bundle" error is suppressed —
			// the unknown-entry error already explains why nothing was loaded.
			assert.NotContains(t, err.Error(), "empty service bundle")
		})
	}
}

func TestExpandServiceBundlesCategoryNameAsFile(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/containers":                &fstest.MapFile{Data: []byte("not a dir")},
		"services/web/networks/internal.network": &fstest.MapFile{Data: []byte("network")},
	}

	_, err := expandServiceBundles(fsys, []string{"web"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "services/web/containers: expected directory")
}

func TestExpandServiceBundlesNestedNonManifest(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/containers/nested":               &fstest.MapFile{Mode: fs.ModeDir},
		"services/web/containers/nested/web.container": &fstest.MapFile{Data: []byte("container")},
	}

	_, err := expandServiceBundles(fsys, []string{"web"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "unsupported nesting")
}

func TestExpandServiceBundlesManifestNestingAllowed(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/manifests/app/config/settings.yml": &fstest.MapFile{Data: []byte("settings")},
	}

	expanded, err := expandServiceBundles(fsys, []string{"web"})
	require.NoError(t, err)
	assert.Equal(t, []manifestRef{
		{
			SrcPath:     "services/web/manifests/app/config/settings.yml",
			LogicalPath: "manifests/app/config/settings.yml",
		},
	}, expanded.Manifests)
}

func TestExpandServiceBundlesRejectsSymlink(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{
			name: "flat subdir symlink",
			fsys: fstest.MapFS{
				"services/web/containers/web.container": &fstest.MapFile{Mode: fs.ModeSymlink},
			},
			want: "services/web/containers/web.container: expected regular file",
		},
		{
			name: "manifest symlink",
			fsys: fstest.MapFS{
				"services/web/manifests/app/deployment.yml": &fstest.MapFile{Mode: fs.ModeSymlink},
			},
			want: "services/web/manifests/app/deployment.yml: expected regular file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := expandServiceBundles(tt.fsys, []string{"web"})
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestExpandServiceBundlesRejectsInvalidName(t *testing.T) {
	t.Parallel()

	// Prepare a filesystem that would be readable under a traversed path, to
	// prove validation fails fast rather than silently loading from `quadlets/`.
	fsys := fstest.MapFS{
		"services/web/containers/web.container": &fstest.MapFile{Data: []byte("[Container]\nImage=a\n")},
		"quadlets/containers/legacy.container":  &fstest.MapFile{Data: []byte("[Container]\nImage=b\n")},
	}

	tests := []struct {
		name    string
		service string
		want    string
	}{
		{"empty", "", "must not be empty"},
		{"dot", ".", `"." is reserved`},
		{"dotdot", "..", `".." is reserved`},
		{"forward slash", "a/b", "must not contain path separators"},
		{"parent traversal", "../quadlets", "must not contain path separators"},
		{"backslash", `a\b`, "must not contain path separators"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := expandServiceBundles(fsys, []string{tt.service})
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestExpandServiceBundlesRejectsBothHookMetadataFiles(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/picolet.yml":              &fstest.MapFile{Data: []byte("secret_hooks: []\n")},
		"services/web/picolet.yml.tmpl":         &fstest.MapFile{Data: []byte("secret_hooks: []\n")},
		"services/web/containers/web.container": &fstest.MapFile{Data: []byte("[Container]\nImage=a\n")},
	}

	_, err := expandServiceBundles(fsys, []string{"web"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "services/web: cannot define both picolet.yml and picolet.yml.tmpl")
}

func TestExpandServiceBundlesHookMetadataAsDirectoryReportsOneError(t *testing.T) {
	t.Parallel()

	// A directory accidentally named picolet.yml previously surfaced both
	// "unknown entry" (from collectBundleSubdirs) and "expected regular file"
	// (from collectBundleHookRefs). Now collectBundleSubdirs skips hook metadata
	// names unconditionally, so only the regular-file error remains.
	fsys := fstest.MapFS{
		"services/web/picolet.yml/something":    &fstest.MapFile{Data: []byte("oops")},
		"services/web/containers/web.container": &fstest.MapFile{Data: []byte("[Container]\nImage=a\n")},
	}

	_, err := expandServiceBundles(fsys, []string{"web"})
	require.ErrorContains(t, err, "services/web/picolet.yml: expected regular file")
	assert.NotContains(t, err.Error(), "unknown entry")
}

func TestAddPathUnknownCategory(t *testing.T) {
	t.Parallel()

	b := &expandedBundles{}
	err := b.addPath("nonsense", "services/web/nonsense/x.yml")
	require.ErrorContains(t, err, `unknown bundle category "nonsense"`)

	// Valid category still works after an invalid one.
	require.NoError(t, b.addPath("container", "services/web/containers/x.container"))
	assert.Equal(t, []string{"services/web/containers/x.container"}, b.Containers)
}

func TestStripServicePrefix(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "manifests/app/deployment.yml.tmpl",
		stripServicePrefix("services/web/manifests/app/deployment.yml.tmpl", "web"))
	assert.Equal(t, "containers/web.container",
		stripServicePrefix("services/web/containers/web.container", "web"))
}
