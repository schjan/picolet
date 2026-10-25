// Package secretref parses the <scheme>://<scope>/<item>/<field> secret
// references shared by the 1Password (op://, scope = vault) and Proton Pass
// (pass://, scope = share) providers.
package secretref

import (
	"fmt"
	"strings"
)

// Ref is a parsed secret reference.
type Ref struct {
	// Scope is the reference's first component: the 1Password vault or the
	// Proton Pass share.
	Scope string
	Item  string
	// Field may contain slashes (e.g. "section/field").
	Field string
}

// Parse parses ref of the form "<prefix><scope>/item/field", where prefix is
// the scheme ("op://") and scopeLabel names the first component in errors
// ("vault"). The field component may contain slashes.
func Parse(prefix, scopeLabel, ref string) (Ref, error) {
	path, ok := strings.CutPrefix(ref, prefix)
	if !ok {
		return Ref{}, fmt.Errorf("invalid %s reference %q: missing %s prefix", prefix, ref, prefix)
	}
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Ref{}, fmt.Errorf("invalid %s reference %q: expected %s%s/item/field", prefix, ref, prefix, scopeLabel)
	}
	return Ref{Scope: parts[0], Item: parts[1], Field: parts[2]}, nil
}

// PodmanSecretName returns the canonical Podman secret name derived from this
// reference: "<scheme>://scope/item/field" produces "scope_item_field", with
// slashes in the field component replaced by underscores.
func (r Ref) PodmanSecretName() string {
	field := strings.ReplaceAll(r.Field, "/", "_")
	return r.Scope + "_" + r.Item + "_" + field
}
