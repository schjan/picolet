package config

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// operatorsFleet is a two-Host Fleet (srv-1 declares the corpus recipient
// srv-1, srv-2 declares none) plus operators.yml when body is not empty.
func operatorsFleet(t *testing.T, body string) fstest.MapFS {
	t.Helper()
	fsys := topologyFleet(agentPorts, map[string]string{
		"srv-1": "hostname: srv-1\nrole: node\nage_recipients:\n  - " + testRecipient(t, "srv-1") + "\n",
		"srv-2": "hostname: srv-2\nrole: node\nuser: pi\n",
	})
	if body != "" {
		fsys[OperatorsFile] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func loadOperators(t *testing.T, fsys fstest.MapFS) (Operators, error) {
	t.Helper()
	cfg, err := LoadAll(fsys)
	require.NoError(t, err)
	return LoadOperators(fsys, cfg)
}

func TestLoadOperators(t *testing.T) {
	t.Parallel()
	op, pq := testRecipient(t, "operator-1"), testRecipient(t, "srv-pq")
	fsys := operatorsFleet(t, "bob:\n  age_recipients:\n    - "+pq+"\n"+
		"alice:\n  age_recipients:\n    - "+strings.ToUpper(op)+"\n")

	ops, err := loadOperators(t, fsys)
	require.NoError(t, err)

	require.Len(t, ops, 2)
	assert.Equal(t, "alice", ops[0].Name, "Operators are sorted by name")
	require.Len(t, ops[0].AgeRecipients, 1)
	assert.Equal(t, op, ops[0].AgeRecipients[0].String(), "recipients are kept in canonical form")
	assert.Equal(t, "bob", ops[1].Name)
	assert.Equal(t, pq, ops[1].AgeRecipients[0].String())
}

func TestLoadOperatorsMissingFileMeansNoOperators(t *testing.T) {
	t.Parallel()
	ops, err := loadOperators(t, operatorsFleet(t, ""))
	require.NoError(t, err)
	assert.Empty(t, ops)
}

func TestLoadOperatorsRejects(t *testing.T) {
	t.Parallel()
	op, srv1 := testRecipient(t, "operator-1"), testRecipient(t, "srv-1")
	tests := []struct{ name, body, want string }{
		{
			name: "unknown key",
			body: "alice:\n  age_recipients:\n    - " + op + "\n  email: a@example.net\n",
			want: "field email not found",
		},
		{
			name: "not a map",
			body: "- alice\n",
			want: "operators.yml",
		},
		{
			name: "malformed recipient",
			body: "alice:\n  age_recipients:\n    - age1notakey\n",
			want: "operators.yml: operator alice: age_recipients[0]: unsupported age recipient",
		},
		{
			name: "SSH recipient",
			body: "alice:\n  age_recipients:\n    - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n",
			want: "SSH recipients are not supported",
		},
		{
			name: "recipient shared with a Host",
			body: "alice:\n  age_recipients:\n    - " + op + "\n    - " + strings.ToUpper(srv1) + "\n",
			want: "operators.yml: operator alice's age recipient " + srv1 + " is also declared by Host srv-1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadOperators(t, operatorsFleet(t, tt.body))
			require.ErrorContains(t, err, tt.want)
		})
	}
}
