package config

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bootstrapFleet is a Fleet whose fleet.yml carries bootstrap, with one Host
// per directory name.
func bootstrapFleet(bootstrap string, hosts map[string]string) fstest.MapFS {
	return topologyFleet(agentPorts+bootstrap, hosts)
}

// The most specific level that declares bootstrap: wins whole: the Host's
// own block, else its Role's, else the Fleet's. An empty block on a level
// cancels the files it would inherit, and is still a declared block.
func TestLoadAllBootstrapPrecedence(t *testing.T) {
	t.Parallel()
	cfg, err := LoadAll(bootstrapFleet(`bootstrap:
  files:
    git_token: op://Infra/fleet-git/token
    mqtt_password: op://Infra/mqtt/password
  roles:
    runner:
      git_token: pass://ci/runner-git/token
    plain: {}
`, map[string]string{
		"fleet-default":  "hostname: fleet-default\nrole: node\nuser: a\nlisten_port: 1\n",
		"role-block":     "hostname: role-block\nrole: runner\nuser: b\nlisten_port: 2\n",
		"role-cancelled": "hostname: role-cancelled\nrole: plain\nuser: c\nlisten_port: 3\n",
		"own-block":      "hostname: own-block\nrole: runner\nuser: d\nlisten_port: 4\nbootstrap:\n  ci_token: pass://ci/own/token\n",
		"own-cancelled":  "hostname: own-cancelled\nrole: node\nuser: e\nlisten_port: 5\nbootstrap: {}\n",
	}))
	require.NoError(t, err)

	assert.Equal(t, BootstrapFiles{ //nolint:gosec // G101: Secret References, not credentials
		"git_token":     "op://Infra/fleet-git/token",
		"mqtt_password": "op://Infra/mqtt/password",
	}, cfg.Hosts["fleet-default"].Bootstrap)
	assert.Equal(t, BootstrapFiles{"git_token": "pass://ci/runner-git/token"}, cfg.Hosts["role-block"].Bootstrap,
		"the Role's block replaces the Fleet's, nothing merged")
	assert.Equal(t, BootstrapFiles{}, cfg.Hosts["role-cancelled"].Bootstrap, "an empty block is declared, not absent")
	assert.Equal(t, BootstrapFiles{"ci_token": "pass://ci/own/token"}, cfg.Hosts["own-block"].Bootstrap, //nolint:gosec // G101: a Secret Reference
		"the Host's block replaces its Role's")
	assert.Equal(t, BootstrapFiles{}, cfg.Hosts["own-cancelled"].Bootstrap, "an empty block is declared, not absent")
}

func TestLoadAllWithoutBootstrap(t *testing.T) {
	t.Parallel()
	cfg, err := LoadAll(bootstrapFleet("", map[string]string{
		"a": "hostname: a\nrole: node\nuser: a\n",
	}))
	require.NoError(t, err)
	assert.Nil(t, cfg.Hosts["a"].Bootstrap)
}

// A bootstrap: entry is a plain file name and a Secret Reference, at every
// level: a value without a provider scheme is most likely a credential
// pasted into git.
func TestLoadAllRejectsInvalidBootstrap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		bootstrap string
		host      string
		want      string
	}{
		{
			name: "host value not a reference",
			host: "bootstrap:\n  git_token: ghp_secret\n",
			want: "host a: bootstrap: git_token is not a Secret Reference (op://vault/item/field or pass://share/item/field)",
		},
		{
			name: "host file name with a directory",
			host: "bootstrap:\n  github/app.pem: op://v/i/f\n",
			want: `host a: bootstrap: "github/app.pem" is not a file name`,
		},
		{
			name: "host file name dot-dot",
			host: "bootstrap:\n  ..: op://v/i/f\n",
			want: `host a: bootstrap: ".." is not a file name`,
		},
		{
			name: "host empty reference",
			host: "bootstrap:\n  git_token: \"\"\n",
			want: "host a: bootstrap: git_token is not a Secret Reference",
		},
		{
			name: "host malformed reference",
			host: "bootstrap:\n  git_token: op://ci/git\n",
			want: "host a: bootstrap: git_token is not a Secret Reference",
		},
		{
			name:      "fleet files",
			bootstrap: "bootstrap:\n  files:\n    git_token: file:///etc/token\n",
			want:      "fleet.yml bootstrap.files: git_token is not a Secret Reference",
		},
		{
			name:      "fleet role",
			bootstrap: "bootstrap:\n  roles:\n    runner:\n      \"\": op://v/i/f\n",
			want:      `fleet.yml bootstrap.roles.runner: "" is not a file name`,
		},
		{
			name:      "fleet unknown key",
			bootstrap: "bootstrap:\n  git_token: op://v/i/f\n",
			want:      "field git_token not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadAll(bootstrapFleet(tt.bootstrap, map[string]string{
				"a": "hostname: a\nrole: node\nuser: a\n" + tt.host,
			}))
			require.ErrorContains(t, err, tt.want)
			require.NotContains(t, err.Error(), "ghp_secret", "a value pasted into git is never echoed")
		})
	}
}
