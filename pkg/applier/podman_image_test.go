package applier

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// splitImageRef feeds images.Tag/Untag, which take repository and tag apart;
// a registry port must not be mistaken for a tag.
func TestSplitImageRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ref, repo, tag string
	}{
		{"localhost/app:latest", "localhost/app", "latest"},
		{"localhost/app:v2", "localhost/app", "v2"},
		{"localhost/app", "localhost/app", "latest"},
		{"registry.lan:5000/app", "registry.lan:5000/app", "latest"},
		{"registry.lan:5000/team/app:1.2", "registry.lan:5000/team/app", "1.2"},
		{"app", "app", "latest"},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			t.Parallel()
			repo, tag := splitImageRef(tt.ref)
			assert.Equal(t, tt.repo, repo)
			assert.Equal(t, tt.tag, tag)
		})
	}
}
