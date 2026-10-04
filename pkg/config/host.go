package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v4"
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
