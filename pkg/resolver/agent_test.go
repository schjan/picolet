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

// The config mount is found however Podman would read its target: cleaned,
// and a relative target under /run/secrets; an env secret mounts nothing.
func TestAgentConfigMountSpellings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		spec string
		want string // "": no config mount
	}{
		{spec: "cfg,target=/etc/picolet/config.yml", want: "cfg"},
		{spec: "cfg,mode=0400,target=/etc/picolet//config.yml", want: "cfg"},
		{spec: "cfg,target=/etc/picolet/./config.yml", want: "cfg"},
		{spec: "cfg,type=mount,target=/etc/picolet/sub/../config.yml", want: "cfg"},
		{spec: "cfg,type=env,target=/etc/picolet/config.yml"},
		{spec: "source=cfg,target=/etc/picolet/config.yml", want: "cfg"},
		{spec: "target=/etc/picolet/config.yml,source=cfg", want: "cfg"},
		{spec: `"cfg,target=/etc/picolet/config.yml"`, want: "cfg"},
		{spec: "other cfg,target=/etc/picolet/config.yml", want: "cfg"},
		{spec: "cfg,target=config.yml"},
		{spec: "cfg"},
	}
	for _, tt := range tests {
		file := agentUnitFile(t, "[Container]\nImage=x\nSecret="+tt.spec+"\n")
		name, ok := AgentConfigMount([]ResolvedFile{file}, "picolet.service")
		assert.Equal(t, tt.want, name, tt.spec)
		assert.Equal(t, tt.want != "", ok, tt.spec)
	}
}

func TestParseSecretMount(t *testing.T) {
	t.Parallel()
	name, target, mounted := ParseSecretMount("tok")
	assert.Equal(t, []any{"tok", "/run/secrets/tok", true}, []any{name, target, mounted})
	name, target, mounted = ParseSecretMount("tok,target=sub/x")
	assert.Equal(t, []any{"tok", "/run/secrets/sub/x", true}, []any{name, target, mounted})
	name, _, mounted = ParseSecretMount("tok,type=env")
	assert.Equal(t, []any{"tok", false}, []any{name, mounted})
	name, target, mounted = ParseSecretMount("mode=0400,source=tok")
	assert.Equal(t, []any{"tok", "/run/secrets/tok", true}, []any{name, target, mounted})
	name, _, mounted = ParseSecretMount("type=env,source=tok")
	assert.Equal(t, []any{"tok", false}, []any{name, mounted})
	name, target, _ = ParseSecretMount("source=tok")
	assert.Equal(t, []any{"source=tok", "/run/secrets/source=tok"}, []any{name, target}, "a value without a comma is the name")
}
