package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
)

func TestLoadAllParsesAgeRecipients(t *testing.T) {
	t.Parallel()
	x25519 := testsops.Recipient(t, "srv-1")
	hybrid := testsops.Recipient(t, "srv-pq")
	fsys := topologyFleet(agentPorts, map[string]string{
		"srv-1": "hostname: srv-1\nrole: node\nage_recipients:\n  - " + strings.ToUpper(x25519) + "\n  - " + hybrid + "\n",
		"srv-2": "hostname: srv-2\nrole: node\nuser: pi\n",
	})

	cfg, err := LoadAll(fsys)
	require.NoError(t, err)

	got := cfg.Hosts["srv-1"].AgeRecipients
	require.Len(t, got, 2)
	assert.Equal(t, x25519, got[0].String(), "recipients are kept in canonical (lowercase) form")
	assert.Equal(t, hybrid, got[1].String())
	assert.Empty(t, cfg.Hosts["srv-2"].AgeRecipients, "age_recipients: is optional")
}

func TestLoadAllRejectsUnsupportedAgeRecipients(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, entry, want string }{
		{name: "malformed", entry: "age1notakey", want: "is not an age recipient"},
		{name: "SSH", entry: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl", want: "SSH recipients are not supported"},
		{name: "plugin", entry: "age1yubikey1qwt50d05nh5vutpdzmlg5wn80xq5negm4uj9ghv0snvdd3yysf5yw3rhl3t", want: "plugin recipients are not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := topologyFleet(agentPorts, map[string]string{
				"srv-1": "hostname: srv-1\nrole: node\nage_recipients:\n  - " + testsops.Recipient(t, "srv-1") + "\n  - " + tt.entry + "\n",
			})
			_, err := LoadAll(fsys)
			require.ErrorContains(t, err, "host srv-1: age_recipients[1]: ")
			require.ErrorContains(t, err, tt.want)
		})
	}
}

// An Agent that predates age_recipients: loads a Fleet that adopts it
// through lenient host loading; that path is the unknown-key path covered by
// TestLoadAllHostUnknownKey. A current Agent knows the key.
func TestLoadAllLenientHostsKnowsAgeRecipients(t *testing.T) {
	t.Parallel()
	fsys := topologyFleet(agentPorts, map[string]string{
		"srv-1": "hostname: srv-1\nrole: node\nage_recipients: [" + testsops.Recipient(t, "srv-1") + "]\n",
	})
	cfg, err := LoadAll(fsys, LenientHosts())
	require.NoError(t, err)
	assert.Len(t, cfg.Hosts["srv-1"].AgeRecipients, 1)
}

// The Agent (lenient host loading) does not reject a malformed
// age_recipients: (spec #248, considered, not adopted): it warns through the
// injected logger and ignores the list, so one bad entry on any Host never
// fails every Agent's reconciliation. Strict loading still fails
// (TestLoadAllRejectsUnsupportedAgeRecipients).
func TestLoadAllLenientHostsIgnoresMalformedAgeRecipients(t *testing.T) {
	t.Parallel()
	fsys := topologyFleet(agentPorts, map[string]string{
		"srv-1": "hostname: srv-1\nrole: node\nage_recipients:\n  - " + testsops.Recipient(t, "srv-1") + "\n  - age1notakey\n",
		"srv-2": "hostname: srv-2\nrole: node\nuser: pi\nage_recipients: [" + testsops.Recipient(t, "srv-2") + "]\n",
	})
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	cfg, err := LoadAll(fsys, LenientHosts(), WithLogger(logger))
	require.NoError(t, err)
	assert.Empty(t, cfg.Hosts["srv-1"].AgeRecipients, "the malformed list is ignored")
	assert.Len(t, cfg.Hosts["srv-2"].AgeRecipients, 1)
	assert.Contains(t, logs.String(), "level=WARN")
	assert.Contains(t, logs.String(), "hosts/srv-1/host.yml")
	assert.Contains(t, logs.String(), "age_recipients[1]")
}

// ageRecipientShapeCases are age_recipients: values that are not a list of
// strings, with the message fragment strict loading names them by.
var ageRecipientShapeCases = []struct{ name, value, want string }{
	{name: "map", value: "\n  srv: age1x\n", want: "age_recipients: must be a list of age recipients"},
	{name: "string", value: " age1x\n", want: "age_recipients: must be a list of age recipients"},
	{name: "nested list", value: "\n  - [age1x]\n", want: "age_recipients[0]: must be a string"},
	{name: "map entry", value: "\n  - {key: age1x}\n", want: "age_recipients[0]: must be a string"},
}

func TestLoadAllRejectsMalformedAgeRecipientsShape(t *testing.T) {
	t.Parallel()
	for _, tt := range ageRecipientShapeCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := topologyFleet(agentPorts, map[string]string{
				"srv-1": "hostname: srv-1\nrole: node\nage_recipients:" + tt.value,
			})
			_, err := LoadAll(fsys)
			require.ErrorContains(t, err, "host srv-1: "+tt.want)
		})
	}
}

// A wrong YAML shape is malformed like a bad entry: lenient host loading
// warns and ignores the list instead of failing the decode of host.yml.
func TestLoadAllLenientHostsIgnoresMalformedAgeRecipientsShape(t *testing.T) {
	t.Parallel()
	for _, tt := range ageRecipientShapeCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := topologyFleet(agentPorts, map[string]string{
				"srv-1": "hostname: srv-1\nrole: node\nage_recipients:" + tt.value,
				"srv-2": "hostname: srv-2\nrole: node\nuser: pi\nage_recipients: [" + testsops.Recipient(t, "srv-2") + "]\n",
			})
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))

			cfg, err := LoadAll(fsys, LenientHosts(), WithLogger(logger))
			require.NoError(t, err)
			assert.Empty(t, cfg.Hosts["srv-1"].AgeRecipients, "the malformed list is ignored")
			assert.Len(t, cfg.Hosts["srv-2"].AgeRecipients, 1)
			assert.Contains(t, logs.String(), "level=WARN")
			assert.Contains(t, logs.String(), "hosts/srv-1/host.yml")
			assert.Contains(t, logs.String(), tt.want)
		})
	}
}

// An empty age_recipients: (null) declares no recipients in both modes.
func TestLoadAllAcceptsEmptyAgeRecipients(t *testing.T) {
	t.Parallel()
	fsys := topologyFleet(agentPorts, map[string]string{
		"srv-1": "hostname: srv-1\nrole: node\nage_recipients:\n",
		"srv-2": "hostname: srv-2\nrole: node\nuser: pi\nage_recipients: []\n",
	})
	cfg, err := LoadAll(fsys)
	require.NoError(t, err)
	assert.Empty(t, cfg.Hosts["srv-1"].AgeRecipients)
	assert.Empty(t, cfg.Hosts["srv-2"].AgeRecipients)
}
