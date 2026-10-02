package resolver

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
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
		"services/web/picolet.yml":                       &fstest.MapFile{Data: []byte("hooks: []\n")},
		"services/web/manifests/app/deployment.yml.tmpl": &fstest.MapFile{Data: []byte("manifest")},
		"services/web/manifests/app/configs/app.conf":    &fstest.MapFile{Data: []byte("config")},
	}

	expanded, err := expandServiceBundles(fsys, []string{"web"})
	require.NoError(t, err)

	assert.ElementsMatch(t, []fileRef{
		{SrcPath: "services/web/networks/internal.network", Category: config.CategoryNetwork},
		{SrcPath: "services/web/systemd/http.socket", Category: config.CategorySystemd},
		{SrcPath: "services/web/volumes/data.volume", Category: config.CategoryVolume},
		{SrcPath: "services/web/containers/web.container.tmpl", Category: config.CategoryContainer},
		{SrcPath: "services/web/kube/app.kube.tmpl", Category: config.CategoryKube},
		{SrcPath: "services/web/secrets/config.yml.tmpl", Category: config.CategorySecret},
		{
			SrcPath:  "services/web/manifests/app/configs/app.conf",
			Category: config.CategoryManifest,
			DataPath: "manifests/app/configs/app.conf",
			RelPath:  "app/configs/app.conf",
		},
		{
			SrcPath:  "services/web/manifests/app/deployment.yml.tmpl",
			Category: config.CategoryManifest,
			DataPath: "manifests/app/deployment.yml.tmpl",
			RelPath:  "app/deployment.yml",
		},
	}, expanded.Files)
	assert.Equal(t, []hookRef{{Service: "web", SrcPath: "services/web/picolet.yml"}}, expanded.Hooks)
}

func TestExpandServiceBundlesMetadataOnlyIsEmpty(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/picolet.yml": &fstest.MapFile{Data: []byte("hooks: []\n")},
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

func TestExpandServiceBundlesIgnoresEmptyDirectory(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/containers":                &fstest.MapFile{Mode: fs.ModeDir},
		"services/web/networks/internal.network": &fstest.MapFile{Data: []byte("network")},
	}

	expanded, err := expandServiceBundles(fsys, []string{"web"})
	require.NoError(t, err)
	assert.Equal(t, []fileRef{
		{SrcPath: "services/web/networks/internal.network", Category: config.CategoryNetwork},
	}, expanded.Files)
}

func TestExpandServiceBundlesRejectsSymlink(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{
			name: "unit symlink",
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
	// prove validation fails fast rather than loading from `quadlets/`.
	fsys := fstest.MapFS{
		"services/web/containers/web.container": &fstest.MapFile{Data: []byte("[Container]\nImage=a\n")},
		"quadlets/other.container":              &fstest.MapFile{Data: []byte("[Container]\nImage=b\n")},
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
		"services/web/picolet.yml":              &fstest.MapFile{Data: []byte("hooks: []\n")},
		"services/web/picolet.yml.tmpl":         &fstest.MapFile{Data: []byte("hooks: []\n")},
		"services/web/containers/web.container": &fstest.MapFile{Data: []byte("[Container]\nImage=a\n")},
	}

	_, err := expandServiceBundles(fsys, []string{"web"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "services/web: cannot define both picolet.yml and picolet.yml.tmpl")
}

func TestExpandServiceBundlesRejectsHookMetadataDirectory(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/picolet.yml/something.network": &fstest.MapFile{Data: []byte("[Network]\n")},
		"services/web/containers/web.container":      &fstest.MapFile{Data: []byte("[Container]\nImage=a\n")},
	}

	_, err := expandServiceBundles(fsys, []string{"web"})
	require.ErrorContains(t, err, "services/web/picolet.yml: expected regular file")
}

func TestExpandServiceBundlesIncludesFilesCategory(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"services/web/containers/web.container": &fstest.MapFile{Data: []byte("c")},
		"services/web/files/scrape.yml":         &fstest.MapFile{Data: []byte("a: 1")},
		"services/web/files/rules/alerts.yml":   &fstest.MapFile{Data: []byte("b: 2")},
	}

	expanded, err := expandServiceBundles(fsys, []string{"web"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []fileRef{
		{SrcPath: "services/web/containers/web.container", Category: config.CategoryContainer},
		{SrcPath: "services/web/files/rules/alerts.yml", Category: config.CategoryFile, DataPath: "files/rules/alerts.yml", RelPath: "rules/alerts.yml"},
		{SrcPath: "services/web/files/scrape.yml", Category: config.CategoryFile, DataPath: "files/scrape.yml", RelPath: "scrape.yml"},
	}, expanded.Files)
}

// bundleTree is a nested, category-free Service Bundle layout; keys are
// bundle-relative.
var bundleTree = map[string]string{
	"web.network":                "[Network]\n",
	"app/web/web.container.tmpl": "[Container]\nImage=app:v1\nEnvironment=HOST={{ .Host.Hostname }}\n",
	"app/backup.timer":           "[Timer]\nOnCalendar=daily\n",
	"app/backup.service":         "[Service]\nExecStart=/bin/true\n",
	"manifests/app/cm.yml":       "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n",
	"files/conf/app.conf.tmpl":   "host={{ .Host.Hostname }}\n",
	"secrets/db.yml":             "password: x\n",
}

// bundleFiles places tree under services/<service>/.
func bundleFiles(service string, tree map[string]string) map[string]string {
	out := make(map[string]string, len(tree))
	for p, content := range tree {
		out["services/"+service+"/"+p] = content
	}
	return out
}

type bundleDeployment struct {
	SrcPath     string
	DestPath    string
	Content     string
	Category    config.Category
	ServiceName string
	RelPath     string
}

// bundleDeployments strips prefix from every SrcPath so a bundle and a
// Fleet-root layout compare equal.
func bundleDeployments(files []ResolvedFile, prefix string) []bundleDeployment {
	out := make([]bundleDeployment, 0, len(files))
	for _, f := range files {
		out = append(out, bundleDeployment{
			SrcPath: strings.TrimPrefix(f.SrcPath, prefix), DestPath: f.DestPath, Content: f.Content,
			Category: f.Category, ServiceName: f.ServiceName, RelPath: f.RelPath,
		})
	}
	return out
}

func TestResolveHostBundleMatchesPathsDirectory(t *testing.T) {
	t.Parallel()
	withMetadata := bundleFiles("web", bundleTree)
	withMetadata["services/web/picolet.yml"] = "hooks: []\n"
	fromBundle, err := resolvePaths(t, "  services: [web]\n", withMetadata)
	require.NoError(t, err)
	fromPaths, err := resolvePaths(t, "  paths: [web.network, app/, manifests/, files/, secrets/]\n", bundleTree)
	require.NoError(t, err)

	assert.ElementsMatch(t, bundleDeployments(fromPaths.Files, ""), bundleDeployments(fromBundle.Files, "services/web/"))
	assert.Len(t, fromBundle.Files, len(bundleTree))
}

// Units deploy from any directory at any depth; a directory named after a
// category does not select it.
func TestResolveHostBundleAcceptsCategoryFreeLayout(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, "  services: [web]\n", bundleFiles("web", map[string]string{
		"web.container":              pathsUnit,
		"stack/db/deep/db.network":   "[Network]\n",
		"containers/nested/a.volume": "[Volume]\n",
	}))
	require.NoError(t, err)
	assert.ElementsMatch(t, []pathsDeployment{
		{SrcPath: "services/web/web.container", DestPath: "/etc/containers/systemd/picolet/web.container", Category: config.CategoryContainer},
		{SrcPath: "services/web/stack/db/deep/db.network", DestPath: "/etc/containers/systemd/picolet/db.network", Category: config.CategoryNetwork},
		{SrcPath: "services/web/containers/nested/a.volume", DestPath: "/etc/containers/systemd/picolet/a.volume", Category: config.CategoryVolume},
	}, pathsDeployments(resolved.Files))
}

func TestResolveHostBundleAppliesPathRules(t *testing.T) {
	t.Parallel()
	for file, want := range map[string]string{
		"app/manifests/cm.yml":   `services/web/app/manifests/cm.yml: "manifests/" must be the first path segment`,
		"manifests/files/cm.yml": `services/web/manifests/files/cm.yml: path has both "manifests/" and "files/"`,
		"README.md":              `services/web/README.md: unknown extension ".md"; move it under files/ or remove it from the listed directory`,
	} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			_, err := resolvePaths(t, "  services: [web]\n", bundleFiles("web", map[string]string{
				file:          "x: 1\n",
				"web.network": "[Network]\n",
			}))
			require.ErrorContains(t, err, want)
		})
	}
}

func TestResolveHostBundleGenericErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  string
		files map[string]string
		want  string
	}{
		{
			name:  "metadata only",
			base:  "  services: [web]\n",
			files: map[string]string{"services/web/picolet.yml": "hooks: []\n"},
			want:  "services/web: empty service bundle",
		},
		{
			name:  "collision inside the bundle",
			base:  "  services: [web]\n",
			files: map[string]string{"services/web/a/web.container": pathsUnit, "services/web/b/web.container.tmpl": pathsUnit},
			want:  "destination collision for /etc/containers/systemd/picolet/web.container: services/web/a/web.container, services/web/b/web.container.tmpl",
		},
		{
			name:  "collision with a paths: entry",
			base:  "  services: [web]\n  paths: [units/]\n",
			files: map[string]string{"services/web/app/web.container": pathsUnit, "units/web.container": pathsUnit},
			want:  "destination collision for /etc/containers/systemd/picolet/web.container: services/web/app/web.container, units/web.container",
		},
		{
			name: "collision across bundles",
			base: "  services: [web, api]\n",
			files: map[string]string{
				"services/web/manifests/app/cm.yml": "kind: ConfigMap\n",
				"services/api/manifests/app/cm.yml": "kind: ConfigMap\n",
			},
			want: "destination collision for /var/lib/picolet/manifests/app/cm.yml: services/api/manifests/app/cm.yml, services/web/manifests/app/cm.yml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolvePaths(t, tt.base, tt.files)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestResolveHostBundleNestedQuadletNamesServiceAndHookUnit(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, "  services: [app]\n", bundleFiles("app", map[string]string{
		"stack/app/deep/app.container.tmpl": "[Container]\nImage=app:v1\nServiceName=app-main\n",
		"secrets/cfg.yml":                   "a: 1\n",
		"picolet.yml": `hooks:
  - name: app-reload
    secrets: [cfg]
    unit: app.container
    action: restart
`,
	}))
	require.NoError(t, err)

	var container *ResolvedFile
	for i := range resolved.Files {
		if resolved.Files[i].Category == config.CategoryContainer {
			container = &resolved.Files[i]
		}
	}
	require.NotNil(t, container)
	assert.Equal(t, "/etc/containers/systemd/picolet/app.container", container.DestPath)
	assert.Equal(t, "app-main.service", container.ServiceName)
	require.Len(t, resolved.Hooks, 1)
	assert.Equal(t, "app-main.service", resolved.Hooks[0].Unit)
}
