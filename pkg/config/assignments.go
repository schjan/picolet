package config

import (
	"cmp"
	"log/slog"
	"slices"
)

// Assignments maps pi_type + features to file sets per host.
type Assignments struct {
	Base     AssignmentGroup            `yaml:"base"`
	PiTypes  map[string]AssignmentGroup `yaml:"pi_types"`
	Features map[string]AssignmentGroup `yaml:"features"`
}

// AggregateSecret describes a Podman secret assembled by globbing files from the repo.
type AggregateSecret struct {
	Name   string `yaml:"name"`
	Glob   string `yaml:"glob"`
	Header string `yaml:"header"`
}

// AssignmentGroup is a collection of file paths grouped by type.
type AssignmentGroup struct {
	Networks         []string          `yaml:"networks"`
	Systemd          []string          `yaml:"systemd"`
	Volumes          []string          `yaml:"volumes"`
	Containers       []string          `yaml:"containers"`
	Kube             []string          `yaml:"kube"`
	Manifests        []string          `yaml:"manifests"`
	Secrets          []string          `yaml:"secrets"`
	AggregateSecrets []AggregateSecret `yaml:"aggregate_secrets"`
}

// ResolvedFileSet is the merged set of all files assigned to a host.
type ResolvedFileSet struct {
	Networks         []string
	Systemd          []string
	Volumes          []string
	Containers       []string
	Kube             []string
	Manifests        []string
	Secrets          []string
	AggregateSecrets []AggregateSecret
}

// Resolve computes the complete file set for a host by merging
// base + pi_type + features assignments.
func (a *Assignments) Resolve(host *HostConfig) *ResolvedFileSet {
	result := &ResolvedFileSet{}
	result.merge(a.Base)
	if group, ok := a.PiTypes[host.PiType]; ok {
		result.merge(group)
	} else if host.PiType != "" {
		slog.Warn("no assignments for pi_type", "pi_type", host.PiType, "host", host.Hostname)
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
	r.Secrets = sortedUnique(r.Secrets)
	r.AggregateSecrets = deduplicateAggregateSecrets(r.AggregateSecrets)
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
	r.Secrets = append(r.Secrets, g.Secrets...)
	r.AggregateSecrets = append(r.AggregateSecrets, g.AggregateSecrets...)
}

// deduplicateAggregateSecrets removes identical (name, glob) pairs, keeping the first
// occurrence in layer order (base → pi_type → features). Different globs for the same name are
// preserved, enabling multiple layers to contribute files to the same aggregate secret.
// SortStableFunc preserves insertion order for equal keys, so the base layer's Header is kept
// when a later layer duplicates the same (name, glob) with a different Header.
func deduplicateAggregateSecrets(entries []AggregateSecret) []AggregateSecret {
	entries = slices.Clone(entries) // avoid mutating the caller's slice
	slices.SortStableFunc(entries, func(a, b AggregateSecret) int {
		if n := cmp.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		return cmp.Compare(a.Glob, b.Glob)
	})
	return slices.CompactFunc(entries, func(a, b AggregateSecret) bool {
		return a.Name == b.Name && a.Glob == b.Glob
	})
}
