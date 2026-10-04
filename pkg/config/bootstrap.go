package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/schjan/picolet/pkg/onepassword"
	"github.com/schjan/picolet/pkg/protonpass"
)

// BootstrapFiles maps a file name in a Host's secrets directory to the
// Secret Reference `picolet bootstrap machine` materializes it from, for a
// Host that runs without a secret provider. Only references live in git,
// never values.
type BootstrapFiles map[string]string

// FleetBootstrap is fleet.yml's bootstrap: the BootstrapFiles of every
// Host, and of the Hosts of a Role. A Host's own bootstrap: wins over its
// Role's, which wins over Files; the winning block replaces the others
// whole, so an empty block cancels an inherited one's files.
type FleetBootstrap struct {
	Files BootstrapFiles            `yaml:"files"`
	Roles map[string]BootstrapFiles `yaml:"roles"`
}

// validate checks every entry is a plain file name and a Secret Reference.
// A value is never part of the error: one that is not a reference is most
// likely a credential pasted into git.
func (b BootstrapFiles) validate() error {
	for _, name := range slices.Sorted(maps.Keys(b)) {
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
			return fmt.Errorf("%q is not a file name", name)
		}
		if ref := b[name]; !onepassword.IsRef(ref) && !protonpass.IsRef(ref) {
			return fmt.Errorf("%s is not a Secret Reference (op://vault/item/field or pass://share/item/field): "+
				"bootstrap: lists references, never values", name)
		}
	}
	return nil
}

func (f *FleetBootstrap) validate() error {
	if err := f.Files.validate(); err != nil {
		return fmt.Errorf("fleet.yml bootstrap.files: %w", err)
	}
	for _, role := range slices.Sorted(maps.Keys(f.Roles)) {
		if err := f.Roles[role].validate(); err != nil {
			return fmt.Errorf("fleet.yml bootstrap.roles.%s: %w", role, err)
		}
	}
	return nil
}

// files is the BootstrapFiles a Host of role inherits from the Fleet: its
// Role's block when fleet.yml declares one, else Files.
func (f *FleetBootstrap) files(role string) BootstrapFiles {
	if files, ok := f.Roles[role]; ok && files != nil {
		return files
	}
	return f.Files
}
