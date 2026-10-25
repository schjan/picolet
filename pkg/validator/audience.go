package validator

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/sops"
)

// audience is the input of the audience pass: who reads each SOPS file and
// who owns each Recipient.
type audience struct {
	// readers maps a SOPS file to the Hosts whose resolve reached it.
	readers map[string][]string
	// hostOwners maps a Recipient to the Hosts declaring it.
	hostOwners map[sops.Recipient][]string
	// operators holds every Operator's Recipient.
	operators map[sops.Recipient]bool
}

// newAudience builds the audience pass's input from every Host's resolve
// and the Operators, or returns nil to skip the pass when a Host in
// unresolved failed to resolve (logged: its files' audiences would be
// incomplete).
func newAudience(cfg *config.Config, resolved []*resolver.ResolvedHost, unresolved []string, ops config.Operators, log *slog.Logger) *audience {
	if len(unresolved) > 0 {
		log.Warn("skipping the SOPS audience pass: some Hosts failed to resolve, so the files' audiences would be incomplete",
			"hosts", strings.Join(unresolved, ", "))
		return nil
	}
	a := &audience{
		readers:    map[string][]string{},
		hostOwners: cfg.HostRecipientOwners(),
		operators:  map[sops.Recipient]bool{},
	}
	for _, rh := range resolved {
		for _, path := range rh.SopsFiles {
			a.readers[path] = append(a.readers[path], rh.Hostname)
		}
	}
	for _, op := range ops {
		for _, r := range op.AgeRecipients {
			a.operators[r] = true
		}
	}
	return a
}

// check fails for every age Recipient of the SOPS file f (at path) that
// belongs neither to an Operator nor to a Host that reaches the file. A file
// no Host reaches allows Operators only.
func (a *audience) check(path string, f *sops.File) []error {
	readers := a.readers[path]
	var errs []error
	for _, g := range f.KeyGroups {
		for _, r := range g.Age {
			if a.operators[r] || a.readsFile(r, readers) {
				continue
			}
			errs = append(errs, audienceError(path, r.String(), a.hostOwners[r], len(readers) == 0))
		}
		// Labels no Host or Operator can declare (SSH, plugin, malformed).
		for _, label := range g.UnsupportedAge {
			errs = append(errs, audienceError(path, label, nil, len(readers) == 0))
		}
	}
	return errs
}

// readsFile reports whether one of r's Hosts is among readers.
func (a *audience) readsFile(r sops.Recipient, readers []string) bool {
	for _, host := range a.hostOwners[r] {
		if slices.Contains(readers, host) {
			return true
		}
	}
	return false
}

// audienceError is the audience pass's error for the Recipient label of the
// file at path, owned by the Hosts owners (none: unknown).
func audienceError(path, label string, owners []string, unreferenced bool) error {
	owner := "unknown"
	if len(owners) > 0 {
		owner = "Host " + strings.Join(owners, ", Host ")
	}
	why := "it is not a Recipient of a Host that references the file or of an Operator"
	if unreferenced {
		why = "no Host references the file, so only Operators may read it"
	}
	return fmt.Errorf("`%s`: age recipient %s (owner: %s) may not read it: %s; "+
		"planned retirement: drop it from the `.sops.yaml` rule and run `sops updatekeys %s`; "+
		"trust lost: drop it, run `sops rotate -i %s` (new data key) and change every value. "+
		"`updatekeys` revokes nothing", path, label, owner, why, path, path)
}

// warnPublishedTestRecipients warns about every published TEST-ONLY
// recipient (picolet's fixture keys) a Host or an Operator declares: its
// identity is public, so whatever is encrypted to it is too.
func warnPublishedTestRecipients(log *slog.Logger, cfg *config.Config, ops config.Operators) {
	const msg = "age recipient is a published test recipient of picolet's fixture corpus; " +
		"its identity is public, so never encrypt a real secret to it"
	warn := func(kind, name string, recipients []sops.Recipient) {
		for _, r := range recipients {
			if sops.IsPublishedTestRecipient(r) {
				log.Warn(msg, kind, name, "recipient", r.String())
			}
		}
	}
	for _, name := range cfg.SortedHostnames() {
		warn("host", name, cfg.Hosts[name].AgeRecipients)
	}
	for _, op := range ops {
		warn("operator", op.Name, op.AgeRecipients)
	}
}
