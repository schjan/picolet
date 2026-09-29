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

	// RetiredPiTypes captures the pre-rename `pi_types:` key so Validate can
	// name the replacement. Reject-only — see keyPresent.
	RetiredPiTypes yaml.Node `yaml:"pi_types"`
}

// Validate rejects retired keys.
func (a *Assignments) Validate() error {
	if keyPresent(a.RetiredPiTypes) {
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
	Pods       []string `yaml:"pods"`
	Manifests  []string `yaml:"manifests"`
	Files      []string `yaml:"files"`
	Secrets    []string `yaml:"secrets"`
	Services   []string `yaml:"services"`
}

// byCategory binds the typed lists of the assignments.yml schema to their
// categories. The lists are the schema itself; #146/#148 replace them with
// extension-derived `paths:` entries.
func (g AssignmentGroup) byCategory() map[Category][]string {
	return map[Category][]string{
		CategoryNetwork:   g.Networks,
		CategorySystemd:   g.Systemd,
		CategoryVolume:    g.Volumes,
		CategoryContainer: g.Containers,
		CategoryKube:      g.Kube,
		CategoryPod:       g.Pods,
		CategoryManifest:  g.Manifests,
		CategoryFile:      g.Files,
		CategorySecret:    g.Secrets,
	}
}

// ResolvedFileSet is the merged set of all files assigned to a host.
type ResolvedFileSet struct {
	// Paths holds the source paths per category, sorted and unique.
	Paths    map[Category][]string
	Services []string
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
	for category, paths := range r.Paths {
		r.Paths[category] = sortedUnique(paths)
	}
	r.Services = sortedUnique(r.Services)
}

// sortedUnique returns a sorted copy with duplicates removed.
func sortedUnique(s []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(s)))
}

func (r *ResolvedFileSet) merge(g AssignmentGroup) {
	if r.Paths == nil {
		r.Paths = make(map[Category][]string)
	}
	for category, paths := range g.byCategory() {
		r.Paths[category] = append(r.Paths[category], paths...)
	}
	r.Services = append(r.Services, g.Services...)
}
