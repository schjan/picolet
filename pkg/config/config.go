package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"slices"

	"go.yaml.in/yaml/v4"
)

// PicoletMarker is the comment header prepended to systemd unit files managed by picolet.
const PicoletMarker = "# Managed by picolet"

// DefaultSelfUnits are the conventional units a picolet agent runs under when
// deployed from its own fleet bundle: "picolet" under the user systemd
// instance, "picolet-system" under the system instance. Stopping or restarting
// one of them synchronously kills the agent before it saves state.
func DefaultSelfUnits() []string {
	return []string{"picolet.service", "picolet-system.service"}
}

// IsDefaultSelfUnit reports whether unit is one of DefaultSelfUnits.
func IsDefaultSelfUnit(unit string) bool {
	return slices.Contains(DefaultSelfUnits(), unit)
}

// Config holds all loaded configuration.
type Config struct {
	Fleet       *FleetConfig
	Hosts       map[string]*HostConfig
	Assignments *Assignments
}

// LoadOption adjusts how LoadAll reads the Fleet.
type LoadOption func(*loadOptions)

type loadOptions struct {
	lenientHosts bool
	logger       *slog.Logger
}

// LenientHosts logs and ignores an unknown key in a host.yml instead of
// failing the load, so an Agent still on an older image keeps reconciling
// when the Fleet adopts a host.yml key only newer Agents know. Only the
// Agent's reconciliation uses it; every other caller (validate, resolve,
// bootstrap) stays strict so typos fail loudly.
func LenientHosts() LoadOption {
	return func(o *loadOptions) { o.lenientHosts = true }
}

// WithLogger sets the logger for load warnings; the default is slog.Default().
func WithLogger(l *slog.Logger) LoadOption {
	return func(o *loadOptions) { o.logger = l }
}

// LoadAll loads fleet.yml, assignments.yml, and all hosts/<name>/host.yml
// from the given filesystem.
func LoadAll(fsys fs.FS, opts ...LoadOption) (*Config, error) {
	o := loadOptions{logger: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}

	fleet, err := loadYAML[FleetConfig](fsys, "fleet.yml")
	if err != nil {
		return nil, fmt.Errorf("loading fleet.yml: %w", err)
	}
	if err := fleet.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	assignments, err := loadYAML[Assignments](fsys, "assignments.yml")
	if err != nil {
		return nil, fmt.Errorf("loading assignments.yml: %w", err)
	}
	if err := assignments.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	hosts, err := loadHosts(fsys, fleet.Ports, o)
	if err != nil {
		return nil, fmt.Errorf("loading hosts: %w", err)
	}

	return &Config{
		Fleet:       fleet,
		Hosts:       hosts,
		Assignments: assignments,
	}, nil
}

// SortedHostnames returns host names in deterministic order.
func (c *Config) SortedHostnames() []string {
	return slices.Sorted(maps.Keys(c.Hosts))
}

// FindHost returns a host by directory key or by the hostname declared in host.yml.
func (c *Config) FindHost(name string) (*HostConfig, bool) {
	if host, ok := c.Hosts[name]; ok {
		return host, true
	}
	for _, host := range c.Hosts {
		if host.Hostname == name {
			return host, true
		}
	}
	return nil, false
}

func loadHosts(fsys fs.FS, ports map[string]int, o loadOptions) (map[string]*HostConfig, error) {
	hosts := make(map[string]*HostConfig)
	entries, err := fs.ReadDir(fsys, "hosts")
	if err != nil {
		return nil, fmt.Errorf("reading hosts directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		hostPath := "hosts/" + name + "/host.yml"
		host, err := loadHostYAML(fsys, hostPath, o)
		if err != nil {
			return nil, err
		}
		if err := host.Validate(); err != nil {
			return nil, fmt.Errorf("host %s: %w", name, err)
		}
		if err := host.applyDefaults(ports); err != nil {
			return nil, fmt.Errorf("host %s: %w", name, err)
		}
		if host.Hostname != name {
			o.logger.Warn("hostname in host.yml does not match directory name",
				"dir", name, "hostname", host.Hostname)
		}
		hosts[name] = host
	}
	if len(hosts) == 0 {
		return nil, errors.New("no hosts found in hosts/ directory")
	}
	if err := validateTopology(hosts); err != nil {
		return nil, err
	}
	return hosts, nil
}

// loadHostYAML parses one host.yml. With lenientHosts, a file that fails only
// because of unknown keys is loaded without them and the strict error is
// logged as a warning; any other error still fails.
func loadHostYAML(fsys fs.FS, path string, o loadOptions) (*HostConfig, error) {
	host, strictErr := loadYAML[HostConfig](fsys, path)
	if strictErr == nil {
		return host, nil
	}
	if !o.lenientHosts {
		return nil, fmt.Errorf("loading %s: %w", path, strictErr)
	}
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, err)
	}
	var lenient HostConfig
	if err := yaml.Load(data, &lenient); err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, strictErr)
	}
	o.logger.Warn("ignoring unknown keys in host.yml; upgrade this Agent's image",
		"path", path, "error", strictErr)
	return &lenient, nil
}

func loadYAML[T any](fsys fs.FS, path string) (*T, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, err
	}
	var v T
	if err := yaml.Load(data, &v, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &v, nil
}
