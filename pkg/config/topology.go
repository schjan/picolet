package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// validateTopology rejects Hosts that would collide on one Machine: two host
// directories declaring the same hostname (the Agent could not tell which one
// is its own), two Agents under the same Linux user on one Machine (including
// two rootful ones), and two Agents listening on the same port on one Machine.
// Machines are compared by MachineKey. Every collision is reported, each
// naming both host directories.
func validateTopology(hosts map[string]*HostConfig) error {
	type machineUser struct{ machine, user string }
	type machinePort struct {
		machine string
		port    int
	}
	byHostname := make(map[string]string, len(hosts))
	byUser := make(map[machineUser]string, len(hosts))
	byPort := make(map[machinePort]string, len(hosts))

	var errs []error
	for _, dir := range slices.Sorted(maps.Keys(hosts)) {
		h := hosts[dir]
		if first, ok := byHostname[h.Hostname]; ok {
			errs = append(errs, fmt.Errorf("hosts %s and %s both declare hostname %q", first, dir, h.Hostname))
		} else {
			byHostname[h.Hostname] = dir
		}

		mu := machineUser{MachineKey(h.Machine), h.User}
		if first, ok := byUser[mu]; ok {
			errs = append(errs, fmt.Errorf("hosts %s and %s both run on machine %q as %s", first, dir, h.Machine, userLabel(h)))
		} else {
			byUser[mu] = dir
		}

		mp := machinePort{MachineKey(h.Machine), h.ListenPort}
		if first, ok := byPort[mp]; ok {
			errs = append(errs, fmt.Errorf("hosts %s and %s both listen on port %d on machine %q", first, dir, h.ListenPort, h.Machine))
		} else {
			byPort[mp] = dir
		}
	}
	return errors.Join(errs...)
}

func userLabel(h *HostConfig) string {
	if h.Rootful() {
		return "the rootful Host"
	}
	return fmt.Sprintf("user %q", h.User)
}
