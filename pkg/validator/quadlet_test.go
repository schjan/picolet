package validator

import (
	"testing"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildUnitInfoContainer(t *testing.T) {
	t.Parallel()

	unit := parser.NewUnitFile()
	unit.Filename = "app.container"
	require.NoError(t, unit.Parse("[Container]\nImage=test\n"))

	info := buildUnitInfo(unit)
	require.NotNil(t, info)
	assert.Equal(t, "app", info.ServiceName)
	// GetContainerResourceName returns "systemd-<name>" (Podman's naming convention)
	assert.Equal(t, "systemd-app", info.ResourceName, "container ResourceName must be pre-filled")
}

func TestBuildUnitInfoNetwork(t *testing.T) {
	t.Parallel()

	unit := parser.NewUnitFile()
	unit.Filename = "internal.network"
	require.NoError(t, unit.Parse("[Network]\n"))

	info := buildUnitInfo(unit)
	require.NotNil(t, info)
	assert.Equal(t, "internal-network", info.ServiceName)
	assert.Empty(t, info.ResourceName, "network ResourceName is set by Convert*, not pre-filled")
}

func TestBuildUnitInfoVolume(t *testing.T) {
	t.Parallel()

	unit := parser.NewUnitFile()
	unit.Filename = "data.volume"
	require.NoError(t, unit.Parse("[Volume]\n"))

	info := buildUnitInfo(unit)
	require.NotNil(t, info)
	assert.Equal(t, "data-volume", info.ServiceName)
	assert.Empty(t, info.ResourceName)
}

func TestBuildUnitInfoKube(t *testing.T) {
	t.Parallel()

	unit := parser.NewUnitFile()
	unit.Filename = "stack.kube"
	require.NoError(t, unit.Parse("[Kube]\nYaml=/manifests/app.yml\n"))

	info := buildUnitInfo(unit)
	require.NotNil(t, info)
	assert.Equal(t, "stack", info.ServiceName)
	assert.Empty(t, info.ResourceName)
}

func TestBuildUnitInfoServiceNameOverride(t *testing.T) {
	t.Parallel()

	unit := parser.NewUnitFile()
	unit.Filename = "app.container"
	require.NoError(t, unit.Parse("[Container]\nServiceName=custom\nImage=test\n"))

	info := buildUnitInfo(unit)
	require.NotNil(t, info)
	assert.Equal(t, "custom", info.ServiceName)
}
