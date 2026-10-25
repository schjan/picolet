package config

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"

	"github.com/schjan/picolet/pkg/sops"
)

// OperatorsFile is the optional Fleet-root file declaring the Operators: the
// people (and their backup keys) who may read every SOPS file besides the
// Hosts that consume it.
const OperatorsFile = "operators.yml"

// Operator is one operators.yml entry: a name and the age Recipients it
// holds.
type Operator struct {
	Name string
	// AgeRecipients is age_recipients: parsed into canonical age recipients,
	// in file order.
	AgeRecipients []sops.Recipient
}

// Operators are the Fleet's Operators, sorted by name.
type Operators []Operator

// operatorYAML is one operators.yml value as written.
type operatorYAML struct {
	AgeRecipients []string `yaml:"age_recipients"`
}

// LoadOperators loads operators.yml from fsys (the Fleet root cfg was loaded
// from): strictly, with host.yml's recipient parser. A missing file means no
// Operators. A Recipient one of cfg's Hosts also declares is an error. Only
// validate reads operators.yml, so neither the Agent nor LoadAll loads it.
func LoadOperators(fsys fs.FS, cfg *Config) (Operators, error) {
	raw, err := loadYAML[map[string]operatorYAML](fsys, OperatorsFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("loading %s: %w", OperatorsFile, err)
	}
	owners := hostRecipientOwners(cfg)
	ops := make(Operators, 0, len(*raw))
	for _, name := range slices.Sorted(maps.Keys(*raw)) {
		recipients, err := parseAgeRecipients((*raw)[name].AgeRecipients)
		if err != nil {
			return nil, fmt.Errorf("%s: operator %s: %w", OperatorsFile, name, err)
		}
		for _, r := range recipients {
			if hosts := owners[r]; len(hosts) > 0 {
				return nil, fmt.Errorf("%s: operator %s's age recipient %s is also declared by Host %s; "+
					"a Recipient belongs to an Operator or to Hosts, never both", OperatorsFile, name, r, hosts[0])
			}
		}
		ops = append(ops, Operator{Name: name, AgeRecipients: recipients})
	}
	return ops, nil
}

// hostRecipientOwners maps each age Recipient cfg's Hosts declare to those
// Hosts, sorted by hostname.
func hostRecipientOwners(cfg *Config) map[sops.Recipient][]string {
	owners := map[sops.Recipient][]string{}
	for _, name := range cfg.SortedHostnames() {
		for _, r := range cfg.Hosts[name].AgeRecipients {
			if !slices.Contains(owners[r], name) {
				owners[r] = append(owners[r], name)
			}
		}
	}
	return owners
}
