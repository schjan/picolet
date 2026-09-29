package config

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
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

// Validate rejects retired keys: `pi_types:`, and the typed lists of every
// group (all of them, base first, then roles and features by name).
func (a *Assignments) Validate() error {
	if keyPresent(a.RetiredPiTypes) {
		return errors.New(migratePiTypes)
	}
	errs := a.Base.retiredListErrors("base")
	for _, role := range slices.Sorted(maps.Keys(a.Roles)) {
		g := a.Roles[role]
		errs = append(errs, g.retiredListErrors("roles."+role)...)
	}
	for _, feature := range slices.Sorted(maps.Keys(a.Features)) {
		g := a.Features[feature]
		errs = append(errs, g.retiredListErrors("features."+feature)...)
	}
	return errors.Join(errs...)
}

// AssignmentGroup is a collection of assignment entries: `paths:` entries
// whose category is derived from the file name (CategoryForPath), `secrets:`
// (the one explicit category) and Service Bundles.
type AssignmentGroup struct {
	// Paths lists Fleet-root-relative files or directories; directories are
	// expanded recursively by the resolver.
	Paths []string `yaml:"paths"`
	// Secrets lists Podman secrets: repo paths, host-only secret file names
	// or provider refs (op://, pass://).
	Secrets  []string `yaml:"secrets"`
	Services []string `yaml:"services"`

	// The retired typed lists, captured so Validate can name `paths:` as the
	// replacement. Reject-only — see keyPresent.
	RetiredNetworks   yaml.Node `yaml:"networks"`
	RetiredSystemd    yaml.Node `yaml:"systemd"`
	RetiredVolumes    yaml.Node `yaml:"volumes"`
	RetiredContainers yaml.Node `yaml:"containers"`
	RetiredKube       yaml.Node `yaml:"kube"`
	RetiredPods       yaml.Node `yaml:"pods"`
	RetiredImages     yaml.Node `yaml:"images"`
	RetiredBuilds     yaml.Node `yaml:"builds"`
	RetiredManifests  yaml.Node `yaml:"manifests"`
	RetiredFiles      yaml.Node `yaml:"files"`
}

// retiredListErrors names every retired typed list present in the group.
func (g *AssignmentGroup) retiredListErrors(group string) []error {
	lists := []struct {
		key  string
		node *yaml.Node
	}{
		{"networks", &g.RetiredNetworks},
		{"systemd", &g.RetiredSystemd},
		{"volumes", &g.RetiredVolumes},
		{"containers", &g.RetiredContainers},
		{"kube", &g.RetiredKube},
		{"pods", &g.RetiredPods},
		{"images", &g.RetiredImages},
		{"builds", &g.RetiredBuilds},
		{"manifests", &g.RetiredManifests},
		{"files", &g.RetiredFiles},
	}
	var errs []error
	for _, l := range lists {
		if keyPresent(*l.node) {
			errs = append(errs, fmt.Errorf(migrateTypedList, group, l.key))
		}
	}
	return errs
}

// ResolvedFileSet is the merged set of assignment entries for a host, each
// list sorted and unique. The resolver expands Paths and Services into
// categorized files.
type ResolvedFileSet struct {
	// Paths holds the `paths:` entries (files or directories).
	Paths    []string
	Secrets  []string
	Services []string
}

// Resolve computes the complete file set for a host by merging
// base + role + features assignments.
func (a *Assignments) Resolve(host *HostConfig) *ResolvedFileSet {
	result := &ResolvedFileSet{}
	result.merge(&a.Base)
	if group, ok := a.Roles[host.Role]; ok {
		result.merge(&group)
	} else if host.Role != "" {
		slog.Warn("no assignments for role", "role", host.Role, "host", host.Hostname)
	}
	for _, feature := range host.Features {
		if group, ok := a.Features[feature]; ok {
			result.merge(&group)
		} else {
			slog.Warn("no assignments for feature", "feature", feature, "host", host.Hostname)
		}
	}
	result.Paths = sortedUnique(result.Paths)
	result.Secrets = sortedUnique(result.Secrets)
	result.Services = sortedUnique(result.Services)
	return result
}

// sortedUnique returns a sorted copy with duplicates removed.
func sortedUnique(s []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(s)))
}

func (r *ResolvedFileSet) merge(g *AssignmentGroup) {
	r.Paths = append(r.Paths, g.Paths...)
	r.Secrets = append(r.Secrets, g.Secrets...)
	r.Services = append(r.Services, g.Services...)
}
