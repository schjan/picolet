package validator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
)

// The containers come first in resolution order on purpose: the built and
// pulled images must still resolve, because their resource names are known
// before any container is converted (Podman prefills a build's ImageTag and
// converts .image units first).
func TestAnalyzeFilesContainerRequiresItsBuildAndImage(t *testing.T) {
	t.Parallel()
	files := []resolver.ResolvedFile{
		newParsedFile(t, config.CategoryContainer, testQuadletDir+"app.container", "[Container]\nImage=app.build\n"),
		newParsedFile(t, config.CategoryContainer, testQuadletDir+"cache.container", "[Container]\nImage=redis.image\n"),
		newParsedFile(t, config.CategoryBuild, testQuadletDir+"app.build",
			"[Build]\nImageTag=localhost/app:latest\nFile=/var/lib/picolet/files/app/Containerfile\nSetWorkingDirectory=file\n"),
		newParsedFile(t, config.CategoryImage, testQuadletDir+"redis.image", "[Image]\nImage=docker.io/library/redis:7\n"),
	}

	depsByUnit, err := AnalyzeFiles(files, Target{})
	require.NoError(t, err)

	assert.Contains(t, depsByUnit["app.service"].Requires, "app-build.service")
	assert.Contains(t, depsByUnit["app.service"].After, "app-build.service")
	assert.Contains(t, depsByUnit["cache.service"].Requires, "redis-image.service")
	assert.Contains(t, depsByUnit["cache.service"].After, "redis-image.service")
}

func TestValidateFilesBuildAndImageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		category config.Category
		file     string
		content  string
		wantErr  string
	}{
		{
			name:     "build without ImageTag fails with Podman's message",
			category: config.CategoryBuild, file: "app.build",
			content: "[Build]\nFile=/srv/Containerfile\n",
			wantErr: "app.build: no ImageTag key specified",
		},
		{
			name:     "build with an unknown key fails with Podman's message",
			category: config.CategoryBuild, file: "app.build",
			content: "[Build]\nImageTag=localhost/app\nFile=/srv/Containerfile\nImage=docker.io/library/app:1\n",
			wantErr: "unsupported key 'Image' in group 'Build'",
		},
		{
			name:     "image without Image fails with Podman's message",
			category: config.CategoryImage, file: "redis.image",
			content: "[Image]\nArch=arm64\n",
			wantErr: "redis.image: no Image key specified",
		},
		{
			name:     "container naming a build absent from the host fails",
			category: config.CategoryContainer, file: "app.container",
			content: "[Container]\nImage=app.build\n",
			wantErr: "requested Quadlet image app.build was not found",
		},
		{
			name:     "container naming an image absent from the host fails",
			category: config.CategoryContainer, file: "cache.container",
			content: "[Container]\nImage=redis.image\n",
			wantErr: "requested Quadlet image redis.image was not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := []resolver.ResolvedFile{newParsedFile(t, tt.category, testQuadletDir+tt.file, tt.content)}
			require.ErrorContains(t, ValidateFiles(files, Target{}), tt.wantErr)
		})
	}
}
