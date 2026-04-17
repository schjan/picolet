package resolver

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollisionKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		srcPath     string
		category    string
		logicalPath string
		want        string
	}{
		{
			name:     "quadlet strips tmpl suffix",
			srcPath:  "services/app/containers/web.container.tmpl",
			category: "container",
			want:     "quadlet/web.container",
		},
		{
			name:     "systemd keeps namespace separate",
			srcPath:  "services/app/systemd/http.socket",
			category: "systemd",
			want:     "systemd/http.socket",
		},
		{
			name:        "manifest uses logical path",
			srcPath:     "services/app/manifests/web/config.yml.tmpl",
			category:    "manifest",
			logicalPath: "manifests/web/config.yml.tmpl",
			want:        "manifest/manifests/web/config.yml",
		},
		{
			name:     "secret strips extension and tmpl suffix",
			srcPath:  "services/app/secrets/config.yaml.tmpl",
			category: "secret",
			want:     "secret/config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, collisionKey(tt.srcPath, tt.category, tt.logicalPath))
		})
	}
}

func TestExpandServiceBundlesHappyPath(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/containers/web.container.tmpl":     &fstest.MapFile{Data: []byte("container")},
		"services/web/volumes/data.volume":               &fstest.MapFile{Data: []byte("volume")},
		"services/web/networks/internal.network":         &fstest.MapFile{Data: []byte("network")},
		"services/web/kube/app.kube.tmpl":                &fstest.MapFile{Data: []byte("kube")},
		"services/web/systemd/http.socket":               &fstest.MapFile{Data: []byte("socket")},
		"services/web/secrets/config.yml.tmpl":           &fstest.MapFile{Data: []byte("secret")},
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
			require.Error(t, err)
			assert.ErrorContains(t, err, "unknown entry")
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

func TestExpandServiceBundlesWithinBundleConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{
			name: "quadlet tmpl variant",
			fsys: fstest.MapFS{
				"services/web/containers/web.container":      &fstest.MapFile{Data: []byte("a")},
				"services/web/containers/web.container.tmpl": &fstest.MapFile{Data: []byte("b")},
			},
			want: "quadlet/web.container",
		},
		{
			name: "quadlet cross category",
			fsys: fstest.MapFS{
				"services/web/containers/web.container": &fstest.MapFile{Data: []byte("a")},
				"services/web/volumes/web.container":    &fstest.MapFile{Data: []byte("b")},
			},
			want: "quadlet/web.container",
		},
		{
			name: "secret extension collision",
			fsys: fstest.MapFS{
				"services/web/secrets/config.yml":  &fstest.MapFile{Data: []byte("a")},
				"services/web/secrets/config.yaml": &fstest.MapFile{Data: []byte("b")},
			},
			want: "secret/config",
		},
		{
			name: "manifest tmpl variant",
			fsys: fstest.MapFS{
				"services/web/manifests/app/deployment.yml":      &fstest.MapFile{Data: []byte("a")},
				"services/web/manifests/app/deployment.yml.tmpl": &fstest.MapFile{Data: []byte("b")},
			},
			want: "manifest/manifests/app/deployment.yml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := expandServiceBundles(tt.fsys, []string{"web"})
			require.ErrorContains(t, err, "bundle conflict")
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestStripServicePrefix(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "manifests/app/deployment.yml.tmpl",
		stripServicePrefix("services/web/manifests/app/deployment.yml.tmpl", "web"))
	assert.Equal(t, "containers/web.container",
		stripServicePrefix("services/web/containers/web.container", "web"))
}
