package validator

import (
	"maps"
	"slices"
	"testing"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
)

// Every extension Podman's Quadlet generator accepts must have a category row,
// so a Podman upgrade that adds an extension fails here instead of deploying
// the file unvalidated. The row's conversion order must be Podman's own.
func TestEveryQuadletExtensionHasCategoryRow(t *testing.T) {
	t.Parallel()
	for ext, order := range quadlet.SupportedExtensions {
		category, ok := config.CategoryForExtension(ext)
		require.True(t, ok, "extension %s has no category", ext)
		spec, ok := config.SpecFor(category)
		require.True(t, ok, "category %s (from %s) has no row", category, ext)
		assert.Equal(t, config.DestQuadlet, spec.Dest, "%s must deploy to the Quadlet directory", ext)
		assert.Equal(t, order, spec.ConvertOrder, "%s conversion order must match quadlet.SupportedExtensions", ext)
	}
}

// The reverse direction: every Quadlet row's extension must be one Podman
// still supports, so a Podman removal or rename fails here instead of leaving
// a row whose files the generator ignores.
func TestEveryQuadletRowExtensionIsSupported(t *testing.T) {
	t.Parallel()
	for _, spec := range config.Specs() {
		if spec.Dest != config.DestQuadlet {
			continue
		}
		var exts []string
		for _, ext := range slices.Sorted(maps.Keys(quadlet.SupportedExtensions)) {
			if category, ok := config.CategoryForExtension(ext); ok && category == spec.Category {
				exts = append(exts, ext)
			}
		}
		assert.NotEmpty(t, exts, "Quadlet row %s has no extension in quadlet.SupportedExtensions", spec.Category)
	}
}

// A CheckQuadlet row without converter wiring would panic in the
// pre-conversion pass; a Prefill row without a resource-name rule would leave
// references to it unresolved.
func TestQuadletRowsWiredToConverters(t *testing.T) {
	t.Parallel()
	for _, spec := range config.Specs() {
		conv, wired := quadletConverters[spec.Category]
		if spec.Check != config.CheckQuadlet {
			assert.False(t, wired, "%s is not CheckQuadlet but has a converter", spec.Category)
			continue
		}
		require.True(t, wired, "%s has no converter", spec.Category)
		assert.NotNil(t, conv.convert, "%s converter", spec.Category)
		assert.Equal(t, spec.Prefill, conv.resourceName != nil, "%s Prefill vs resource-name rule", spec.Category)
	}
}

func TestArtifactQuadletRejected(t *testing.T) {
	t.Parallel()
	unit := parser.NewUnitFile()
	unit.Filename = "data.artifact"
	require.NoError(t, unit.Parse("[Artifact]\nArtifact=quay.io/example/data:latest\n"))

	err := ValidateFiles([]resolver.ResolvedFile{{
		DestPath:   "/etc/containers/systemd/picolet/data.artifact",
		Category:   config.CategoryArtifact,
		ParsedUnit: unit,
	}}, Target{})
	require.ErrorContains(t, err, "`.artifact` is not supported by picolet")
}
