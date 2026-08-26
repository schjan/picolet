package config

import (
	"errors"
	"log/slog"
	"slices"

	"go.yaml.in/yaml/v4"
)

// Assignments maps role + features to file sets per host.
type Assignments struct {
	Base     AssignmentGroup            `yaml:"base"`
	Roles    map[string]AssignmentGroup `yaml:"roles"`
	Features map[string]AssignmentGroup `yaml:"features"`

	// DeprecatedPiTypes captures the pre-rename `pi_types:` key so Validate can
	// name the replacement. Reject-only — see keyPresent.
	DeprecatedPiTypes yaml.Node `yaml:"pi_types"`
}

// Validate rejects renamed keys.
func (a *Assignments) Validate() error {
	if keyPresent(a.DeprecatedPiTypes) {
		return errors.New(migratePiTypes)
	}
	return nil
}

// AssignmentGroup is a collection of file paths grouped by type.
type AssignmentGroup struct {
	Networks   []string `yaml:"networks"`
	Systemd    []string `yaml:"systemd"`
	Volumes    []string `yaml:"volumes"`
	Containers []string `yaml:"containers"`
	Kube       []string `yaml:"kube"`
	Manifests  []string `yaml:"manifests"`
	Files      []string `yaml:"files"`
	Secrets    []string `yaml:"secrets"`
	Services   []string `yaml:"services"`
}

// ResolvedFileSet is the merged set of all files assigned to a host.
type ResolvedFileSet struct {
	Networks   []string
	Systemd    []string
	Volumes    []string
	Containers []string
	Kube       []string
	Manifests  []string
	Files      []string
	Secrets    []string
	Services   []string
}

// Resolve computes the complete file set for a host by merging
// base + role + features assignments.
func (a *Assignments) Resolve(host *HostConfig) *ResolvedFileSet {
	result := &ResolvedFileSet{}
	result.merge(a.Base)
	if group, ok := a.Roles[host.Role]; ok {
		result.merge(group)
	} else if host.Role != "" {
		slog.Warn("no assignments for role", "role", host.Role, "host", host.Hostname)
	}
	for _, feature := range host.Features {
		if group, ok := a.Features[feature]; ok {
			result.merge(group)
		} else {
			slog.Warn("no assignments for feature", "feature", feature, "host", host.Hostname)
		}
	}
	result.deduplicate()
	return result
}

func (r *ResolvedFileSet) deduplicate() {
	r.Networks = sortedUnique(r.Networks)
	r.Systemd = sortedUnique(r.Systemd)
	r.Volumes = sortedUnique(r.Volumes)
	r.Containers = sortedUnique(r.Containers)
	r.Kube = sortedUnique(r.Kube)
	r.Manifests = sortedUnique(r.Manifests)
	r.Files = sortedUnique(r.Files)
	r.Secrets = sortedUnique(r.Secrets)
	r.Services = sortedUnique(r.Services)
}

// sortedUnique returns a sorted copy with duplicates removed.
func sortedUnique(s []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(s)))
}

func (r *ResolvedFileSet) merge(g AssignmentGroup) {
	r.Networks = append(r.Networks, g.Networks...)
	r.Systemd = append(r.Systemd, g.Systemd...)
	r.Volumes = append(r.Volumes, g.Volumes...)
	r.Containers = append(r.Containers, g.Containers...)
	r.Kube = append(r.Kube, g.Kube...)
	r.Manifests = append(r.Manifests, g.Manifests...)
	r.Files = append(r.Files, g.Files...)
	r.Secrets = append(r.Secrets, g.Secrets...)
	r.Services = append(r.Services, g.Services...)
}
