package config

import (
	"bytes"
	"log/slog"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentPorts is the fleet.yml ports block the reference Agent templates render.
const agentPorts = "ports:\n  picolet_metrics: 9417\n  picolet_system_metrics: 9418\n"

// topologyFleet builds a minimal Fleet with the given fleet.yml ports block and
// one host.yml per directory name.
func topologyFleet(ports string, hosts map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{
		"fleet.yml":       &fstest.MapFile{Data: []byte("images: {}\n" + ports)},
		"assignments.yml": &fstest.MapFile{Data: []byte("base: {}\nroles: {}\nfeatures: {}\n")},
	}
	for dir, body := range hosts {
		fsys["hosts/"+dir+"/host.yml"] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func TestLoadAllHostTopologyDefaults(t *testing.T) {
	t.Parallel()
	fsys := topologyFleet(agentPorts, map[string]string{
		"vps":        "hostname: vps\nrole: node\nuser: pi\n",
		"vps-system": "hostname: vps-system\nrole: node\nmachine: vps\n",
		"vps-runner": "hostname: vps-runner\nrole: node\nmachine: vps\nuser: runner\nlisten_port: 9419\n",
	})

	cfg, err := LoadAll(fsys)
	require.NoError(t, err)

	user := cfg.Hosts["vps"]
	assert.Equal(t, "vps", user.Machine, "machine defaults to the hostname")
	assert.Equal(t, "pi", user.User)
	assert.False(t, user.Rootful())
	assert.Equal(t, 9417, user.ListenPort, "a user Host defaults to ports.picolet_metrics")

	system := cfg.Hosts["vps-system"]
	assert.Equal(t, "vps", system.Machine)
	assert.Empty(t, system.User, "no user: means the rootful Host")
	assert.True(t, system.Rootful())
	assert.Equal(t, 9418, system.ListenPort, "the rootful Host defaults to ports.picolet_system_metrics")

	runner := cfg.Hosts["vps-runner"]
	assert.Equal(t, "runner", runner.User)
	assert.Equal(t, 9419, runner.ListenPort, "a declared listen_port wins")
}

func TestLoadAllRejectsMissingDefaultListenPort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ports   string
		host    string
		wantErr string
	}{
		{
			name:    "user Host",
			ports:   "ports: {}\n",
			host:    "hostname: vps\nrole: node\nuser: pi\n",
			wantErr: "host vps: listen_port: is not set and fleet.yml ports has no picolet_metrics key",
		},
		{
			name:    "rootful Host",
			ports:   "ports: {}\n",
			host:    "hostname: vps\nrole: node\n",
			wantErr: "host vps: listen_port: is not set and fleet.yml ports has no picolet_system_metrics key",
		},
		{
			name:    "Fleet default out of range",
			ports:   "ports:\n  picolet_system_metrics: 0\n",
			host:    "hostname: vps\nrole: node\n",
			wantErr: "host vps: fleet.yml ports.picolet_system_metrics (the default listen_port) must be between 1 and 65535: 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadAll(topologyFleet(tt.ports, map[string]string{"vps": tt.host}))
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestLoadAllRejectsInvalidHostTopology(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		host    string
		wantErr string
	}{
		{
			name:    "user name with uppercase",
			host:    "hostname: vps\nrole: node\nuser: Runner\n",
			wantErr: `host vps: user "Runner" is not a valid Linux user name`,
		},
		{
			name:    "user name starting with a digit",
			host:    "hostname: vps\nrole: node\nuser: 1runner\n",
			wantErr: `host vps: user "1runner" is not a valid Linux user name`,
		},
		{
			name:    "user name longer than 32 characters",
			host:    "hostname: vps\nrole: node\nuser: abcdefghijklmnopqrstuvwxyzabcdefg\n",
			wantErr: `host vps: user "abcdefghijklmnopqrstuvwxyzabcdefg" is not a valid Linux user name`,
		},
		{
			name:    "root spelled out",
			host:    "hostname: vps\nrole: node\nuser: root\n",
			wantErr: "host vps: user: root is the rootful Host; omit user: instead",
		},
		{
			name:    "machine with a dot",
			host:    "hostname: vps\nrole: node\nmachine: vps.example.net\n",
			wantErr: `host vps: machine "vps.example.net" is not a valid hostname label`,
		},
		{
			name:    "machine ending in a hyphen",
			host:    "hostname: vps\nrole: node\nmachine: vps-\n",
			wantErr: `host vps: machine "vps-" is not a valid hostname label`,
		},
		{
			name:    "machine defaulted from an invalid hostname",
			host:    "hostname: vps.example.net\nrole: node\n",
			wantErr: `host vps: machine "vps.example.net" (defaulted from hostname) is not a valid hostname label; declare machine: explicitly`,
		},
		{
			name:    "listen_port zero",
			host:    "hostname: vps\nrole: node\nlisten_port: 0\n",
			wantErr: "host vps: listen_port must be between 1 and 65535: 0",
		},
		{
			name:    "listen_port above range",
			host:    "hostname: vps\nrole: node\nlisten_port: 65536\n",
			wantErr: "host vps: listen_port must be between 1 and 65535: 65536",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadAll(topologyFleet(agentPorts, map[string]string{"vps": tt.host}))
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestLoadAllAcceptsValidHostTopologyNames(t *testing.T) {
	t.Parallel()
	fsys := topologyFleet(agentPorts, map[string]string{
		"a": "hostname: a\nrole: node\nmachine: VPS-1\nuser: _svc-ci_2\nlisten_port: 1\n",
		"b": "hostname: b\nrole: node\nmachine: vps-1\nuser: abcdefghijklmnopqrstuvwxyzabcdef\nlisten_port: 65535\n",
	})
	_, err := LoadAll(fsys)
	require.NoError(t, err)
}

func TestLoadAllRejectsDuplicateHostTopology(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		hosts   map[string]string
		wantErr string
	}{
		{
			name: "same hostname in two directories",
			hosts: map[string]string{
				"vps":     "hostname: vps\nrole: node\n",
				"vps-old": "hostname: vps\nrole: node\nmachine: old\n",
			},
			wantErr: `hosts vps and vps-old both declare hostname "vps"`,
		},
		{
			name: "two rootful Hosts on one Machine",
			hosts: map[string]string{
				"vps":        "hostname: vps\nrole: node\nlisten_port: 9000\n",
				"vps-system": "hostname: vps-system\nrole: node\nmachine: vps\n",
			},
			wantErr: `hosts vps and vps-system both run on machine "vps" as the rootful Host`,
		},
		{
			name: "same user on one Machine",
			hosts: map[string]string{
				"vps-ci":     "hostname: vps-ci\nrole: node\nmachine: vps\nuser: ci\n",
				"vps-runner": "hostname: vps-runner\nrole: node\nmachine: vps\nuser: ci\nlisten_port: 9500\n",
			},
			wantErr: `hosts vps-ci and vps-runner both run on machine "vps" as user "ci"`,
		},
		{
			name: "same effective port on one Machine",
			hosts: map[string]string{
				"vps":        "hostname: vps\nrole: node\nuser: pi\n",
				"vps-runner": "hostname: vps-runner\nrole: node\nmachine: vps\nuser: runner\n",
			},
			wantErr: `hosts vps and vps-runner both listen on port 9417 on machine "vps"`,
		},
		{
			name: "Machine names differing only in case are one Machine",
			hosts: map[string]string{
				"vps":        "hostname: vps\nrole: node\nlisten_port: 9000\n",
				"vps-system": "hostname: vps-system\nrole: node\nmachine: VPS\n",
			},
			wantErr: `hosts vps and vps-system both run on machine "VPS" as the rootful Host`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadAll(topologyFleet(agentPorts, tt.hosts))
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestLoadAllAllowsSameUserAndPortOnDifferentMachines(t *testing.T) {
	t.Parallel()
	_, err := LoadAll(topologyFleet(agentPorts, map[string]string{
		"a":        "hostname: a\nrole: node\nuser: pi\n",
		"b":        "hostname: b\nrole: node\nuser: pi\n",
		"a-system": "hostname: a-system\nrole: node\nmachine: a\n",
		"b-system": "hostname: b-system\nrole: node\nmachine: b\n",
	}))
	require.NoError(t, err)
}

func TestLoadAllHostUnknownKey(t *testing.T) {
	t.Parallel()
	fleet := func() fstest.MapFS {
		return topologyFleet(agentPorts, map[string]string{
			"vps": "hostname: vps\nrole: node\nfuture_key: 1\n",
		})
	}

	t.Run("Agent loads it with a warning", func(t *testing.T) {
		t.Parallel()
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))

		cfg, err := LoadAll(fleet(), LenientHosts(), WithLogger(logger))
		require.NoError(t, err)
		assert.Equal(t, "node", cfg.Hosts["vps"].Role)
		assert.Contains(t, logs.String(), "level=WARN")
		assert.Contains(t, logs.String(), "hosts/vps/host.yml")
		assert.Contains(t, logs.String(), "future_key")
	})

	t.Run("every other caller rejects it", func(t *testing.T) {
		t.Parallel()
		_, err := LoadAll(fleet())
		require.ErrorContains(t, err, "future_key")
	})

	t.Run("a malformed value still fails the Agent", func(t *testing.T) {
		t.Parallel()
		fsys := topologyFleet(agentPorts, map[string]string{
			"vps": "hostname: vps\nrole: node\nfuture_key: 1\nlisten_port: nine\n",
		})
		_, err := LoadAll(fsys, LenientHosts())
		require.ErrorContains(t, err, "hosts/vps/host.yml")
	})
}
