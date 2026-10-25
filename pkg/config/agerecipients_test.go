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

// TestLoadAllMalformedAgeRecipients covers bad entries and wrong YAML shapes
// of age_recipients: in both modes. Strict loading fails naming the field.
// The Agent (lenient host loading) does not reject a malformed
// age_recipients: (spec #248, considered, not adopted): it warns through the
// injected logger and ignores the list, so one bad entry on any Host never
// fails every Agent's reconciliation; a wrong YAML shape is malformed like a
// bad entry and does not fail the decode of host.yml either.
func TestLoadAllMalformedAgeRecipients(t *testing.T) {
	t.Parallel()
	// entry is a bad second entry after a valid one; value is a whole
	// age_recipients: value of the wrong shape.
	tests := []struct {
		name, entry, value string
		wantPrefix         string   // follows "host srv-1: " in strict errors
		want               []string // further fragments of the error
	}{
		{name: "malformed", entry: "age1notakey", wantPrefix: "age_recipients[1]: ", want: []string{"is not an age recipient"}},
		{name: "SSH", entry: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl", wantPrefix: "age_recipients[1]: ", want: []string{"SSH recipients are not supported"}},
		{name: "plugin", entry: "age1yubikey1qwt50d05nh5vutpdzmlg5wn80xq5negm4uj9ghv0snvdd3yysf5yw3rhl3t", wantPrefix: "age_recipients[1]: ", want: []string{"plugin recipients are not supported"}},
		{name: "map", value: "\n  srv: age1x\n", wantPrefix: "age_recipients: must be a list of age recipients"},
		{name: "string", value: " age1x\n", wantPrefix: "age_recipients: must be a list of age recipients"},
		{name: "nested list", value: "\n  - [age1x]\n", wantPrefix: "age_recipients[0]: must be a string"},
		{name: "map entry", value: "\n  - {key: age1x}\n", wantPrefix: "age_recipients[0]: must be a string"},
	}
	for _, mode := range []string{"strict", "lenient"} {
		lenient := mode == "lenient"
		for _, tt := range tests {
			t.Run(mode+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				value := tt.value
				if tt.entry != "" {
					value = "\n  - " + testsops.Recipient(t, "srv-1") + "\n  - " + tt.entry + "\n"
				}
				fsys := topologyFleet(agentPorts, map[string]string{
					"srv-1": "hostname: srv-1\nrole: node\nage_recipients:" + value,
					"srv-2": "hostname: srv-2\nrole: node\nuser: pi\nage_recipients: [" + testsops.Recipient(t, "srv-2") + "]\n",
				})
				if !lenient {
					_, err := LoadAll(fsys)
					require.ErrorContains(t, err, "host srv-1: "+tt.wantPrefix)
					for _, frag := range tt.want {
						require.ErrorContains(t, err, frag)
					}
					return
				}
				var logs bytes.Buffer
				cfg, err := LoadAll(fsys, LenientHosts(), WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
				require.NoError(t, err)
				assert.Empty(t, cfg.Hosts["srv-1"].AgeRecipients, "the malformed list is ignored")
				assert.Len(t, cfg.Hosts["srv-2"].AgeRecipients, 1)
				for _, frag := range append([]string{"level=WARN", "hosts/srv-1/host.yml", tt.wantPrefix}, tt.want...) {
					assert.Contains(t, logs.String(), frag)
				}
			})
		}
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
