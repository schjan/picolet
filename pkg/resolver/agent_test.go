package resolver

import (
	"testing"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
)

func agentUnitFile(t *testing.T, content string) ResolvedFile {
	t.Helper()
	unit := parser.NewUnitFile()
	require.NoError(t, unit.Parse(content))
	return ResolvedFile{ServiceName: "picolet.service", ParsedUnit: unit}
}

func TestAgentConfigSecretNotRendered(t *testing.T) {
	t.Parallel()
	tests := map[string][]ResolvedFile{
		"no mount":                {agentUnitFile(t, "[Container]\nImage=x\n")},
		"secret not in the Fleet": {agentUnitFile(t, "[Container]\nImage=x\nSecret=cfg,target="+AgentConfigTarget+"\n")},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := AgentConfigSecret(files, "picolet.service")
			require.ErrorIs(t, err, ErrAgentConfigNotRendered)
		})
	}
}

func TestAgentConfigSecretFound(t *testing.T) {
	t.Parallel()
	files := []ResolvedFile{
		agentUnitFile(t, "[Container]\nImage=x\nSecret=cfg,target="+AgentConfigTarget+"\n"),
		{Category: config.CategorySecret, DestPath: "secret:cfg", Content: "hostname: h\n"},
	}
	file, err := AgentConfigSecret(files, "picolet.service")
	require.NoError(t, err)
	assert.Equal(t, "secret:cfg", file.DestPath)
}
