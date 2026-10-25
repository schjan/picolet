package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/schjan/picolet/pkg/sops"
)

const maxPort = 65535

var (
	// linuxUserName is the portable subset useradd accepts by default
	// (NAME_REGEX), capped at the 32-character utmp limit.
	linuxUserName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	// hostnameLabel is one RFC 1123 hostname label. Labels are
	// case-insensitive, so Machines are grouped by MachineKey.
	hostnameLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

// Fleet ports keys the reference Agent config templates render as the
// Agent's listen port; a Host without listen_port: takes its port from them.
const (
	userAgentPortKey    = "picolet_metrics"
	rootfulAgentPortKey = "picolet_system_metrics"
)

// HostConfig holds per-host configuration from hosts/<hostname>/host.yml.
type HostConfig struct {
	Hostname         string   `yaml:"hostname"`
	ExternalHostname string   `yaml:"external_hostname"`
	Role             string   `yaml:"role"`
	Features         []string `yaml:"features"`

	// Machine is the Machine this Host runs on. LoadAll defaults it to
	// Hostname, so consumers never see it empty.
	Machine string `yaml:"machine"`
	// User is the Linux user the Host's Agent runs as; empty for the rootful
	// Host (see Rootful).
	User string `yaml:"user"`
	// ListenPort is the port the Host's Agent listens on: listen_port: when
	// declared, otherwise the Fleet's ports entry for the Agent (see
	// userAgentPortKey, rootfulAgentPortKey). Set by LoadAll.
	ListenPort int `yaml:"-"`
	// DeclaredListenPort is listen_port: as written; nil when absent.
	DeclaredListenPort *int `yaml:"listen_port"`

	// Bootstrap is the files bootstrap machine materializes for the Host:
	// bootstrap: when declared, otherwise its Role's or the Fleet's from
	// fleet.yml (see FleetBootstrap). Nil when no level declares a block:
	// only such a Host runs with a secret provider; an empty block
	// (bootstrap: {}) keeps it off the provider with nothing to place. Set
	// by LoadAll.
	Bootstrap BootstrapFiles `yaml:"-"`
	// DeclaredBootstrap is bootstrap: as written; nil when absent.
	DeclaredBootstrap BootstrapFiles `yaml:"bootstrap"`

	// AgeRecipients is age_recipients: parsed into canonical age recipients
	// (X25519 or hybrid post-quantum), in file order; empty when the Host
	// declares none. Set by LoadAll.
	AgeRecipients []sops.Recipient `yaml:"-"`
	// DeclaredAgeRecipients is age_recipients: as written, kept as a node so
	// a wrong YAML shape reaches applyAgeRecipients (which lenient loading
	// turns into a warning) instead of failing the decode of host.yml.
	DeclaredAgeRecipients yaml.Node `yaml:"age_recipients"`

	// RetiredPiType captures the pre-rename `pi_type:` key so Validate can
	// reject it with a migration message instead of the generic unknown-field
	// error WithKnownFields() would produce. Reject-only — see keyPresent.
	RetiredPiType yaml.Node `yaml:"pi_type"`
}

// Rootful reports whether the Host's Agent runs as root (no user: declared).
func (h *HostConfig) Rootful() bool {
	return h.User == ""
}

// MachineKey is the identity of a Machine name: hostname labels are
// case-insensitive, so "VPS-1" and "vps-1" are one Machine. Group and compare
// Machines by it; keep Machine itself for display.
func MachineKey(machine string) string {
	return strings.ToLower(machine)
}

// Validate checks that required fields are present, that no retired key is
// used, and that the topology fields are well-formed.
func (h *HostConfig) Validate() error {
	if keyPresent(h.RetiredPiType) {
		return errors.New(migratePiType)
	}
	if h.Hostname == "" {
		return errors.New("hostname is required")
	}
	if h.Role == "" {
		return errors.New("role is required")
	}
	if err := h.validateUser(); err != nil {
		return err
	}
	if err := h.validateMachine(); err != nil {
		return err
	}
	if p := h.DeclaredListenPort; p != nil && !validPort(*p) {
		return fmt.Errorf("listen_port must be between 1 and %d: %d", maxPort, *p)
	}
	if err := h.DeclaredBootstrap.validate(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return nil
}

// parseAgeRecipients parses age_recipients: entries with the SOPS package's
// parser; an error names the entry by its index (never by its value, which
// may be a pasted secret key).
func parseAgeRecipients(entries []string) ([]sops.Recipient, error) {
	out := make([]sops.Recipient, 0, len(entries))
	for i, entry := range entries {
		r, err := sops.ParseRecipient(entry)
		if err != nil {
			return nil, fmt.Errorf("age_recipients[%d]: %w", i, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func (h *HostConfig) validateUser() error {
	switch {
	case h.User == "root":
		return errors.New("user: root is the rootful Host; omit user: instead")
	case h.User != "" && !linuxUserName.MatchString(h.User):
		return fmt.Errorf("user %q is not a valid Linux user name (lowercase letters, digits, '_' and '-', not starting with a digit or '-', at most 32 characters)", h.User)
	}
	return nil
}

// validateMachine checks machine: as its effective value, so a Hostname that
// cannot serve as a Machine name is reported with a hint to declare machine:.
func (h *HostConfig) validateMachine() error {
	switch {
	case h.Machine == "" && !hostnameLabel.MatchString(h.Hostname):
		return fmt.Errorf("machine %q (defaulted from hostname) is not a valid hostname label; declare machine: explicitly", h.Hostname)
	case h.Machine != "" && !hostnameLabel.MatchString(h.Machine):
		return fmt.Errorf("machine %q is not a valid hostname label (letters, digits and '-', not starting or ending with '-', at most 63 characters)", h.Machine)
	}
	return nil
}

// ageRecipientEntries returns age_recipients: as its list of strings: nil
// when absent or null, an error naming the key when it is not a list or an
// entry is not a string (never quoting a value, which may be a pasted secret
// key).
func ageRecipientEntries(n *yaml.Node) ([]string, error) {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	if n.IsZero() || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null") {
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, errors.New("age_recipients: must be a list of age recipients")
	}
	entries := make([]string, 0, len(n.Content))
	for i, item := range n.Content {
		if item.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("age_recipients[%d]: must be a string", i)
		}
		entries = append(entries, item.Value)
	}
	return entries, nil
}

// applyAgeRecipients fills AgeRecipients from age_recipients:, the one place
// it is parsed. A malformed list (a wrong YAML shape or a bad entry) fails
// the load, except under lenient host loading (the Agent), which warns and
// ignores the whole list: one bad entry on any Host must not fail every
// Agent's reconciliation.
func (h *HostConfig) applyAgeRecipients(path string, o loadOptions) error {
	entries, err := ageRecipientEntries(&h.DeclaredAgeRecipients)
	var recipients []sops.Recipient
	if err == nil {
		recipients, err = parseAgeRecipients(entries)
	}
	if err != nil {
		if !o.lenientHosts {
			return err
		}
		o.logger.Warn("ignoring malformed age_recipients: in host.yml", "path", path, "error", err)
		return nil
	}
	h.AgeRecipients = recipients
	return nil
}

// applyDefaults fills Machine, Bootstrap and ListenPort from the Hostname
// and the Fleet. A Host with no listen_port: whose Agent port key is missing
// from the Fleet's ports is an error: the Fleet is the sole owner of the
// Agent's port.
func (h *HostConfig) applyDefaults(fleet *FleetConfig) error {
	if h.Machine == "" {
		h.Machine = h.Hostname
	}
	h.Bootstrap = h.DeclaredBootstrap
	if h.Bootstrap == nil {
		h.Bootstrap = fleet.Bootstrap.files(h.Role)
	}
	if h.DeclaredListenPort != nil {
		h.ListenPort = *h.DeclaredListenPort
		return nil
	}
	key := userAgentPortKey
	if h.Rootful() {
		key = rootfulAgentPortKey
	}
	port, ok := fleet.Ports[key]
	if !ok {
		return fmt.Errorf("listen_port: is not set and fleet.yml ports has no %s key", key)
	}
	if !validPort(port) {
		return fmt.Errorf("fleet.yml ports.%s (the default listen_port) must be between 1 and %d: %d", key, maxPort, port)
	}
	h.ListenPort = port
	return nil
}

func validPort(p int) bool {
	return p >= 1 && p <= maxPort
}
