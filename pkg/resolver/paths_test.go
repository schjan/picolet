package resolver

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
)

const pathsUnit = "[Container]\nImage=app:v1\n\n[Install]\nWantedBy=default.target\n"

// pathsFleetFS builds a single-host fleet whose base group is the given
// assignments.yml body; extra maps Fleet-root-relative paths to content.
func pathsFleetFS(base string, extra map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{
		"fleet.yml":                &fstest.MapFile{Data: []byte("images: {}\nports: {}\n")},
		"assignments.yml":          &fstest.MapFile{Data: []byte("base:\n" + base + "roles: {}\nfeatures: {}\n")},
		"hosts/test-host/host.yml": &fstest.MapFile{Data: []byte("hostname: test-host\nexternal_hostname: test-host.example.net\nrole: node\nfeatures: []\n")},
	}
	for p, content := range extra {
		fsys[p] = &fstest.MapFile{Data: []byte(content)}
	}
	return fsys
}

func resolvePaths(t *testing.T, base string, extra map[string]string) (*ResolvedHost, error) {
	t.Helper()
	fsys := pathsFleetFS(base, extra)
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := New(Config{FS: fsys, Config: cfg})
	require.NoError(t, err)
	return r.ResolveHost(t.Context(), "test-host")
}

type pathsDeployment struct {
	SrcPath  string
	DestPath string
	Category config.Category
	RelPath  string
}

func pathsDeployments(files []ResolvedFile) []pathsDeployment {
	out := make([]pathsDeployment, 0, len(files))
	for _, f := range files {
		out = append(out, pathsDeployment{SrcPath: f.SrcPath, DestPath: f.DestPath, Category: f.Category, RelPath: f.RelPath})
	}
	return out
}

func TestResolveHostPathsDirectoryCategoryByExtension(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, "  paths:\n    - units/\n", map[string]string{
		"units/web/web.container": pathsUnit,
		"units/web/web.pod":       "[Pod]\n",
		"units/backup.timer":      "[Timer]\nOnCalendar=daily\n",
		"units/backup.service":    "[Service]\nExecStart=/bin/true\n",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []pathsDeployment{
		{SrcPath: "units/web/web.container", DestPath: "/etc/containers/systemd/picolet/web.container", Category: config.CategoryContainer},
		{SrcPath: "units/web/web.pod", DestPath: "/etc/containers/systemd/picolet/web.pod", Category: config.CategoryPod},
		{SrcPath: "units/backup.timer", DestPath: "/etc/systemd/system/backup.timer", Category: config.CategorySystemd},
		{SrcPath: "units/backup.service", DestPath: "/etc/systemd/system/backup.service", Category: config.CategorySystemd},
	}, pathsDeployments(resolved.Files))
}

func TestResolveHostPathsFirstSegmentSelectsCategory(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, "  paths:\n    - manifests/\n    - files/\n    - secrets/db.yml\n", map[string]string{
		"manifests/app/deploy.yml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n",
		"files/app/app.conf":       "key=value\n",
		"files/web.container":      pathsUnit,
		"secrets/db.yml":           "password: x\n",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []pathsDeployment{
		{SrcPath: "manifests/app/deploy.yml", DestPath: "/var/lib/picolet/manifests/app/deploy.yml", Category: config.CategoryManifest, RelPath: "app/deploy.yml"},
		{SrcPath: "files/app/app.conf", DestPath: "/var/lib/picolet/files/app/app.conf", Category: config.CategoryFile, RelPath: "app/app.conf"},
		{SrcPath: "files/web.container", DestPath: "/var/lib/picolet/files/web.container", Category: config.CategoryFile, RelPath: "web.container"},
		{SrcPath: "secrets/db.yml", DestPath: "secret:db", Category: config.CategorySecret},
	}, pathsDeployments(resolved.Files))
}

func TestResolveHostPathsCategorySegmentMustBeFirst(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		file  string
		wants []string
	}{
		{
			name:  "deep manifests",
			file:  "app/manifests/deploy.yml",
			wants: []string{"app/manifests/deploy.yml: ", `"manifests/" must be the first path segment`},
		},
		{
			name:  "deep files",
			file:  "app/files/web.container",
			wants: []string{"app/files/web.container: ", `"files/" must be the first path segment`},
		},
		{
			name:  "deep secrets",
			file:  "app/secrets/db.yml",
			wants: []string{"app/secrets/db.yml: ", `"secrets/" must be the first path segment`},
		},
		{
			name:  "two segments",
			file:  "manifests/files/deploy.yml",
			wants: []string{"manifests/files/deploy.yml: ", `both "manifests/" and "files/"`},
		},
		{
			name:  "repeated manifests",
			file:  "manifests/manifests/deploy.yml",
			wants: []string{"manifests/manifests/deploy.yml: ", `"manifests/" must be the first path segment`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			top, _, _ := strings.Cut(tt.file, "/")
			_, err := resolvePaths(t, "  paths:\n    - "+top+"\n", map[string]string{tt.file: "x: 1\n"})
			require.Error(t, err)
			for _, want := range tt.wants {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

func TestResolveHostPathsTmplStrippedFromFileNameOnly(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, "  paths:\n    - units/\n    - files/\n", map[string]string{
		"units/web.pod.tmpl":             "[Pod]\n",
		"units/stack.tmpl/web.container": pathsUnit,
		"files/conf.tmpl/app.conf.tmpl":  "port=8080\n",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []pathsDeployment{
		{SrcPath: "units/web.pod.tmpl", DestPath: "/etc/containers/systemd/picolet/web.pod", Category: config.CategoryPod},
		{SrcPath: "units/stack.tmpl/web.container", DestPath: "/etc/containers/systemd/picolet/web.container", Category: config.CategoryContainer},
		{SrcPath: "files/conf.tmpl/app.conf.tmpl", DestPath: "/var/lib/picolet/files/conf.tmpl/app.conf", Category: config.CategoryFile, RelPath: "conf.tmpl/app.conf"},
	}, pathsDeployments(resolved.Files))
}

func TestResolveHostPathsRejectsUncategorizableFiles(t *testing.T) {
	t.Parallel()
	for file, want := range map[string]string{
		"units/web.tmpl":      "no file extension", // .tmpl is not an extension
		"units/Containerfile": "no file extension",
		"units/LICENSE":       "no file extension",
		"units/.gitkeep":      "no file extension",
		"units/README.md":     `unknown extension ".md"`,
		"units/app.contaner":  `unknown extension ".contaner"`,
	} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			_, err := resolvePaths(t, "  paths:\n    - units/\n", map[string]string{
				file:                pathsUnit,
				"units/web.network": "[Network]\n",
			})
			require.Error(t, err)
			assert.ErrorContains(t, err, file+": "+want+"; move it under files/ or remove it from the listed directory")
		})
	}
}

func TestResolveHostPathsSkipsBundleMetadata(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, "  paths:\n    - units/\n", map[string]string{
		"units/picolet.yml":          "hooks: []\n",
		"units/web/picolet.yml.tmpl": "hooks: []\n",
		"units/web/web.network":      "[Network]\n",
	})
	require.NoError(t, err)
	assert.Equal(t, []pathsDeployment{
		{SrcPath: "units/web/web.network", DestPath: "/etc/containers/systemd/picolet/web.network", Category: config.CategoryNetwork},
	}, pathsDeployments(resolved.Files))
}

func TestResolveHostPathsCoexistWithTypedLists(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, `  containers: [units/web.container]
  manifests: [manifests/app/deploy.yml]
  secrets: [secrets/db.yml]
  paths:
    - units/
    - units/web.container
    - manifests/
    - secrets/
`, map[string]string{
		"units/web.container":      pathsUnit,
		"manifests/app/deploy.yml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n",
		"secrets/db.yml":           "password: x\n",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []pathsDeployment{
		{SrcPath: "units/web.container", DestPath: "/etc/containers/systemd/picolet/web.container", Category: config.CategoryContainer},
		{SrcPath: "manifests/app/deploy.yml", DestPath: "/var/lib/picolet/manifests/app/deploy.yml", Category: config.CategoryManifest, RelPath: "app/deploy.yml"},
		{SrcPath: "secrets/db.yml", DestPath: "secret:db", Category: config.CategorySecret},
	}, pathsDeployments(resolved.Files))
}

// A typed list and `paths:` may select one source in two categories when the
// destinations differ (a staged migration can deploy it twice on purpose).
func TestResolveHostPathsTypedCategoryWithDistinctDestinationDeploysBoth(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, `  secrets: [files/token]
  files: [units/web.container]
  paths: [files/token, units/web.container]
`, map[string]string{
		"files/token":         "s3cr3t\n",
		"units/web.container": pathsUnit,
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []pathsDeployment{
		{SrcPath: "files/token", DestPath: "secret:token", Category: config.CategorySecret},
		{SrcPath: "files/token", DestPath: "/var/lib/picolet/files/token", Category: config.CategoryFile, RelPath: "token"},
		{SrcPath: "units/web.container", DestPath: "/var/lib/picolet/units/web.container", Category: config.CategoryFile, RelPath: "units/web.container"},
		{SrcPath: "units/web.container", DestPath: "/etc/containers/systemd/picolet/web.container", Category: config.CategoryContainer},
	}, pathsDeployments(resolved.Files))
}

func TestResolveHostPathsDestinationCollision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  string
		files map[string]string
		want  string
	}{
		{
			name:  "two directories, one unit name",
			base:  "  paths: [a/, b/]\n",
			files: map[string]string{"a/web.container": pathsUnit, "b/web.container": pathsUnit},
			want:  "destination collision for /etc/containers/systemd/picolet/web.container: a/web.container, b/web.container",
		},
		{
			name:  "typed list and paths",
			base:  "  containers: [quadlets/web.container]\n  paths: [units/]\n",
			files: map[string]string{"quadlets/web.container": pathsUnit, "units/web.container.tmpl": pathsUnit},
			want:  "destination collision for /etc/containers/systemd/picolet/web.container: quadlets/web.container, units/web.container.tmpl",
		},
		{
			name:  "typed list and paths disagree on the category",
			base:  "  files: [manifests/deploy.yml]\n  paths: [manifests/]\n",
			files: map[string]string{"manifests/deploy.yml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n"},
			want:  "manifests/deploy.yml: a typed list selects it as file, a paths: entry as manifest",
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

func TestResolveHostPathsEntryMustExistUnderFleetRoot(t *testing.T) {
	t.Parallel()
	for entry, want := range map[string]string{
		"/etc/units":  `paths entry "/etc/units": must be relative to the Fleet root`,
		"../units":    `paths entry "../units": must be relative to the Fleet root`,
		"units/ghost": "units/ghost",
	} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			_, err := resolvePaths(t, "  paths: [\""+entry+"\"]\n", map[string]string{"units/web.network": "[Network]\n"})
			require.ErrorContains(t, err, want)
		})
	}
}

// #147 expands a Service Bundle through expandTree with its services/<name>/
// prefix stripped; the logical paths must then categorize exactly like a
// `paths:` directory with the same contents.
func TestExpandTreeBundlePrefixMatchesPathsDirectory(t *testing.T) {
	t.Parallel()
	tree := map[string]string{
		"web.container.tmpl":   pathsUnit,
		"units/web.network":    "[Network]\n",
		"manifests/app/cm.yml": "kind: ConfigMap\n",
		"files/conf/app.conf":  "k=v\n",
		"secrets/db.yml":       "password: x\n",
		"picolet.yml":          "hooks: []\n",
	}
	fsys := fstest.MapFS{}
	for p, content := range tree {
		fsys["services/web/"+p] = &fstest.MapFile{Data: []byte(content)}
		fsys[p] = &fstest.MapFile{Data: []byte(content)}
	}

	bundle := &expandedBundles{}
	require.NoError(t, bundle.expandTree(fsys, "services/web", "services/web/"))
	fromPaths, err := expandPathEntries(fsys, []string{"web.container.tmpl", "units", "manifests", "files", "secrets", "picolet.yml"})
	require.NoError(t, err)

	for category, srcs := range bundle.Paths {
		for i, src := range srcs {
			srcs[i] = strings.TrimPrefix(src, "services/web/")
		}
		assert.ElementsMatch(t, fromPaths.Paths[category], srcs, category)
	}
	assert.Len(t, bundle.Paths, len(fromPaths.Paths))
	for i := range bundle.NestedRefs {
		bundle.NestedRefs[i].SrcPath = strings.TrimPrefix(bundle.NestedRefs[i].SrcPath, "services/web/")
	}
	assert.ElementsMatch(t, fromPaths.NestedRefs, bundle.NestedRefs)
}
