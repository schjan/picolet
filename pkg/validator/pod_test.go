package validator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
)

const testQuadletDir = "/etc/containers/systemd/picolet/"

// The pod file comes first in resolution order on purpose: its members must
// still be listed, because conversion follows Podman's order (pods last), not
// the order files were resolved in.
func TestAnalyzeFilesPodListsMemberContainers(t *testing.T) {
	t.Parallel()
	files := []resolver.ResolvedFile{
		newParsedFile(t, config.CategoryPod, testQuadletDir+"web.pod", "[Pod]\nPublishPort=8080:80\n"),
		newParsedFile(t, config.CategoryContainer, testQuadletDir+"api.container", "[Container]\nImage=docker.io/library/api:1\nPod=web.pod\n"),
		newParsedFile(t, config.CategoryContainer, testQuadletDir+"proxy.container", "[Container]\nImage=docker.io/library/nginx:1\nPod=web.pod\n"),
		newParsedFile(t, config.CategoryContainer, testQuadletDir+"solo.container", "[Container]\nImage=docker.io/library/solo:1\n"),
	}

	depsByUnit, err := AnalyzeFiles(files, false)
	require.NoError(t, err)

	pod := depsByUnit["web-pod.service"]
	assert.Subset(t, pod.Wants, []string{"api.service", "proxy.service"})
	assert.NotContains(t, pod.Wants, "solo.service")
	assert.Equal(t, []string{"api.service", "proxy.service"}, pod.Before)
	assert.Contains(t, depsByUnit["api.service"].BindsTo, "web-pod.service")
	assert.Contains(t, depsByUnit["proxy.service"].After, "web-pod.service")
	assert.NotContains(t, depsByUnit["solo.service"].BindsTo, "web-pod.service")
}

func TestValidateFilesPodErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   func(t *testing.T) []resolver.ResolvedFile
		wantErr string
	}{
		{
			name: "malformed pod fails with Podman's message",
			files: func(t *testing.T) []resolver.ResolvedFile {
				t.Helper()
				return []resolver.ResolvedFile{
					newParsedFile(t, config.CategoryPod, testQuadletDir+"web.pod", "[Pod]\nImage=docker.io/library/nginx:1\n"),
				}
			},
			wantErr: "unsupported key 'Image' in group 'Pod'",
		},
		{
			name: "container naming a pod absent from the host fails",
			files: func(t *testing.T) []resolver.ResolvedFile {
				t.Helper()
				return []resolver.ResolvedFile{
					newParsedFile(t, config.CategoryContainer, testQuadletDir+"api.container", "[Container]\nImage=docker.io/library/api:1\nPod=web.pod\n"),
				}
			},
			wantErr: "quadlet pod unit web.pod does not exist",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.ErrorContains(t, ValidateFiles(tt.files(t), false), tt.wantErr)
		})
	}
}
