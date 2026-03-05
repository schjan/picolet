package validator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:funlen // table-driven test: rows are data, not complexity
func TestUnitNameFromContent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		filename string
		content  string
		want     string
	}{
		{
			name:     "container default",
			filename: "app.container",
			content:  "[Container]\nImage=test\n",
			want:     "app.service",
		},
		{
			name:     "network default",
			filename: "internal.network",
			content:  "[Network]\n",
			want:     "internal-network.service",
		},
		{
			name:     "volume default",
			filename: "data.volume",
			content:  "[Volume]\n",
			want:     "data-volume.service",
		},
		{
			name:     "kube default",
			filename: "stack.kube",
			content:  "[Kube]\nYaml=/manifests/app.yml\n",
			want:     "stack.service",
		},
		{
			name:     "container ServiceName override",
			filename: "app.container",
			content:  "[Container]\nServiceName=custom\nImage=test\n",
			want:     "custom.service",
		},
		{
			name:     "network ServiceName override",
			filename: "net.network",
			content:  "[Network]\nServiceName=my-net\n",
			want:     "my-net.service",
		},
		{
			name:     "non-quadlet extension",
			filename: "unit.service",
			content:  "[Service]\nExecStart=/bin/true\n",
			want:     "",
		},
		{
			// Empty content still parses; service name is derived from the filename.
			name:     "empty content",
			filename: "empty.container",
			content:  "",
			want:     "empty.service",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := UnitNameFromContent(tc.filename, tc.content)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestUnitNameFromFile(t *testing.T) {
	t.Parallel()

	t.Run("reads from disk", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "myapp.container")
		require.NoError(t, os.WriteFile(path, []byte("[Container]\nImage=test\n"), 0o600))
		assert.Equal(t, "myapp.service", UnitNameFromFile(path))
	})

	t.Run("respects ServiceName override", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "app.container")
		require.NoError(t, os.WriteFile(path, []byte("[Container]\nServiceName=override\nImage=test\n"), 0o600))
		assert.Equal(t, "override.service", UnitNameFromFile(path))
	})

	t.Run("missing file returns empty", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, UnitNameFromFile("/nonexistent/path/missing.container"))
	})
}
