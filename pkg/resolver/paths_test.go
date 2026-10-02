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
		"fleet.yml":                &fstest.MapFile{Data: []byte("images: {}\nports: {picolet_system_metrics: 9418}\n")},
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
	resolved, err := resolvePaths(t, "  paths:\n    - services/\n", map[string]string{
		"services/web/picolet.yml":      "hooks: []\n",
		"services/api/picolet.yml.tmpl": "hooks: []\n",
		"services/web/web.network":      "[Network]\n",
	})
	require.NoError(t, err)
	assert.Equal(t, []pathsDeployment{
		{SrcPath: "services/web/web.network", DestPath: "/etc/containers/systemd/picolet/web.network", Category: config.CategoryNetwork},
	}, pathsDeployments(resolved.Files))
}

// Only a bundle's root metadata is skipped: picolet.yml anywhere else is
// ordinary content and takes the normal categorization.
func TestResolveHostPathsPicoletYmlOutsideBundleRootIsOrdinary(t *testing.T) {
	t.Parallel()

	t.Run("files/ entry resolves as a File", func(t *testing.T) {
		t.Parallel()
		resolved, err := resolvePaths(t, "  paths: [files/picolet.yml]\n", map[string]string{
			"files/picolet.yml": "listen: :9418\n",
		})
		require.NoError(t, err)
		assert.Equal(t, []pathsDeployment{
			{SrcPath: "files/picolet.yml", DestPath: "/var/lib/picolet/files/picolet.yml", Category: config.CategoryFile, RelPath: "picolet.yml"},
		}, pathsDeployments(resolved.Files))
	})

	t.Run("nested in a listed directory", func(t *testing.T) {
		t.Parallel()
		resolved, err := resolvePaths(t, "  paths: [files/]\n", map[string]string{
			"files/agent/picolet.yml.tmpl": "host: {{ .Host.Hostname }}\n",
		})
		require.NoError(t, err)
		assert.Equal(t, []pathsDeployment{
			{SrcPath: "files/agent/picolet.yml.tmpl", DestPath: "/var/lib/picolet/files/agent/picolet.yml", Category: config.CategoryFile, RelPath: "agent/picolet.yml"},
		}, pathsDeployments(resolved.Files))
		assert.Equal(t, "host: test-host\n", resolved.Files[0].Content)
	})

	t.Run("in a bundle subdirectory", func(t *testing.T) {
		t.Parallel()
		resolved, err := resolvePaths(t, "  services: [agent]\n", map[string]string{
			"services/agent/picolet.yml":       "hooks: []\n",
			"services/agent/files/picolet.yml": "listen: :9418\n",
		})
		require.NoError(t, err)
		assert.Equal(t, []pathsDeployment{
			{SrcPath: "services/agent/files/picolet.yml", DestPath: "/var/lib/picolet/files/picolet.yml", Category: config.CategoryFile, RelPath: "picolet.yml"},
		}, pathsDeployments(resolved.Files))
		assert.Empty(t, resolved.Hooks)
	})

	t.Run("under a directory that cannot be a bundle", func(t *testing.T) {
		t.Parallel()
		_, err := resolvePaths(t, "  paths: [services/]\n", map[string]string{
			"services/a\\b/picolet.yml": "hooks: []\n",
		})
		require.ErrorContains(t, err, `unknown extension ".yml"`)
	})

	t.Run("direct root entry errors", func(t *testing.T) {
		t.Parallel()
		_, err := resolvePaths(t, "  paths: [picolet.yml]\n", map[string]string{
			"picolet.yml": "hooks: []\n",
		})
		require.ErrorContains(t, err, `picolet.yml: unknown extension ".yml"`)
	})
}

// A file reached twice — by two `paths:` entries, by a `paths:` entry and the
// `secrets:` list, or by a Service Bundle and a `paths:` entry — deploys once.
func TestResolveHostPathsFileReachedTwiceDeploysOnce(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, `  secrets: [secrets/db.yml]
  services: [web]
  paths:
    - units/
    - units/web.container
    - manifests/
    - secrets/
    - services/web/web.network
`, map[string]string{
		"units/web.container":      pathsUnit,
		"manifests/app/deploy.yml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n",
		"secrets/db.yml":           "password: x\n",
		"services/web/web.network": "[Network]\n",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []pathsDeployment{
		{SrcPath: "units/web.container", DestPath: "/etc/containers/systemd/picolet/web.container", Category: config.CategoryContainer},
		{SrcPath: "services/web/web.network", DestPath: "/etc/containers/systemd/picolet/web.network", Category: config.CategoryNetwork},
		{SrcPath: "manifests/app/deploy.yml", DestPath: "/var/lib/picolet/manifests/app/deploy.yml", Category: config.CategoryManifest, RelPath: "app/deploy.yml"},
		{SrcPath: "secrets/db.yml", DestPath: "secret:db", Category: config.CategorySecret},
	}, pathsDeployments(resolved.Files))
}

// The `secrets:` list and `paths:` may select one source in two categories:
// the destinations differ, so both deploy.
func TestResolveHostPathsSecretAndFileOfOneSourceDeployBoth(t *testing.T) {
	t.Parallel()
	resolved, err := resolvePaths(t, `  secrets: [files/token]
  paths: [files/token]
`, map[string]string{
		"files/token": "s3cr3t\n",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []pathsDeployment{
		{SrcPath: "files/token", DestPath: "secret:token", Category: config.CategorySecret},
		{SrcPath: "files/token", DestPath: "/var/lib/picolet/files/token", Category: config.CategoryFile, RelPath: "token"},
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
			name:  "secrets list and paths",
			base:  "  secrets: [host/db.yml]\n  paths: [secrets/]\n",
			files: map[string]string{"secrets/db.yml": "password: x\n"},
			want:  "destination collision for secret:db: host/db.yml, secrets/db.yml",
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
		"/etc/units":     `paths entry "/etc/units": must be relative to the Fleet root`,
		"../units":       `paths entry "../units": must be relative to the Fleet root`,
		"units/../units": `paths entry "units/../units": must be relative to the Fleet root`,
		"units/ghost":    "units/ghost",
	} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			_, err := resolvePaths(t, "  paths: [\""+entry+"\"]\n", map[string]string{"units/web.network": "[Network]\n"})
			require.ErrorContains(t, err, want)
		})
	}
}
