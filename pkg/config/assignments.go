package config

import (
	"log/slog"
	"slices"
)

// Assignments maps pi_type + features to file sets per host.
type Assignments struct {
	Base     AssignmentGroup            `yaml:"base"`
	PiTypes  map[string]AssignmentGroup `yaml:"pi_types"`
	Features map[string]AssignmentGroup `yaml:"features"`
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

// categorySlices returns pointers to the per-category path slices. Its order
// must match AssignmentGroup.categorySlices — the two structs carry the same
// nine fields and these two methods are the only code that walks them.
func (r *ResolvedFileSet) categorySlices() []*[]string {
	return []*[]string{
		&r.Networks, &r.Systemd, &r.Volumes, &r.Containers, &r.Kube,
		&r.Manifests, &r.Files, &r.Secrets, &r.Services,
	}
}

// categorySlices returns the per-category path slices in the same order as
// ResolvedFileSet.categorySlices.
func (g AssignmentGroup) categorySlices() [][]string {
	return [][]string{
		g.Networks, g.Systemd, g.Volumes, g.Containers, g.Kube,
		g.Manifests, g.Files, g.Secrets, g.Services,
	}
}

func (r *ResolvedFileSet) deduplicate() {
	for _, s := range r.categorySlices() {
		*s = sortedUnique(*s)
	}
}

// sortedUnique returns a sorted copy with duplicates removed.
func sortedUnique(s []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(s)))
}

func (r *ResolvedFileSet) merge(g AssignmentGroup) {
	dst := r.categorySlices()
	for i, src := range g.categorySlices() {
		*dst[i] = append(*dst[i], src...)
	}
}
