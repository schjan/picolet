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

// Config holds all loaded configuration.
type Config struct {
	Fleet       *FleetConfig
	Hosts       map[string]*HostConfig
	Assignments *Assignments
}

// LoadAll loads fleet.yml, assignments.yml, and all hosts/<name>/host.yml
// from the given filesystem.
func LoadAll(fsys fs.FS) (*Config, error) {
	fleet, err := loadYAML[FleetConfig](fsys, "fleet.yml")
	if err != nil {
		return nil, fmt.Errorf("loading fleet.yml: %w", err)
	}

	assignments, err := loadYAML[Assignments](fsys, "assignments.yml")
	if err != nil {
		return nil, fmt.Errorf("loading assignments.yml: %w", err)
	}

	hosts, err := loadHosts(fsys)
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

func loadHosts(fsys fs.FS) (map[string]*HostConfig, error) {
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
		host, err := loadYAML[HostConfig](fsys, hostPath)
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", hostPath, err)
		}
		if err := host.Validate(); err != nil {
			return nil, fmt.Errorf("host %s: %w", name, err)
		}
		if host.Hostname != name {
			slog.Warn("hostname in host.yml does not match directory name",
				"dir", name, "hostname", host.Hostname)
		}
		hosts[name] = host
	}
	if len(hosts) == 0 {
		return nil, errors.New("no hosts found in hosts/ directory")
	}
	return hosts, nil
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
