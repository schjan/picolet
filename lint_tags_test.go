package picolet_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

// testOnlyTags are the build tags that select test sources only; lint adds them
// to the shared BUILD_TAGS so it covers the E2E and provider integration tests.
var testOnlyTags = []string{"e2e", "onepassword_integration", "protonpass_integration"}

// TestLintBuildTags pins .golangci.yml's run.build-tags (the one lint tag set,
// read by CI and `task lint` alike) to the shared BUILD_TAGS of Taskfile.yml
// and the CI workflow plus the test-only tags, so lint cannot drift from them.
func TestLintBuildTags(t *testing.T) {
	t.Parallel()

	var lint struct {
		Run struct {
			BuildTags []string `yaml:"build-tags"`
		} `yaml:"run"`
	}
	readYAML(t, ".golangci.yml", &lint)

	var task struct {
		Vars struct {
			BuildTags string `yaml:"BUILD_TAGS"`
		} `yaml:"vars"`
	}
	readYAML(t, "Taskfile.yml", &task)

	var ci struct {
		Env struct {
			BuildTags string `yaml:"BUILD_TAGS"`
		} `yaml:"env"`
	}
	readYAML(t, ".github/workflows/ci.yml", &ci)

	require.Equal(t, task.Vars.BuildTags, ci.Env.BuildTags, "Taskfile.yml and ci.yml BUILD_TAGS differ")
	want := slices.Concat(strings.Split(task.Vars.BuildTags, ","), testOnlyTags)
	require.ElementsMatch(t, want, lint.Run.BuildTags, ".golangci.yml run.build-tags must be BUILD_TAGS plus the test-only tags")
}

func readYAML(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(data, v), "parse %s", path)
}
