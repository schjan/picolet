package onepassword

import (
	"fmt"
	"strings"
)

// Prefix is the URI scheme prefix for 1Password secret references.
const Prefix = "op://"

// IsRef reports whether s is a 1Password secret reference.
func IsRef(s string) bool { return strings.HasPrefix(s, Prefix) }

// OpRef is a parsed op:// secret reference.
type OpRef struct {
	Vault string
	Item  string
	Field string
}

// ParseOpRef parses a 1Password secret reference of the form "op://vault/item/field".
// The field component may contain slashes (e.g. "op://vault/item/section/field").
func ParseOpRef(ref string) (OpRef, error) {
	path, ok := strings.CutPrefix(ref, Prefix)
	if !ok {
		return OpRef{}, fmt.Errorf("invalid op:// reference %q: missing op:// prefix", ref)
	}
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return OpRef{}, fmt.Errorf("invalid op:// reference %q: expected op://vault/item/field", ref)
	}
	return OpRef{
		Vault: parts[0],
		Item:  parts[1],
		Field: parts[2],
	}, nil
}

// PodmanSecretName returns the canonical Podman secret name derived from this reference.
// "op://vault/item/field" produces "item_field".
// Slashes in the field component are replaced with underscores.
func (r OpRef) PodmanSecretName() string {
	field := strings.ReplaceAll(r.Field, "/", "_")
	return r.Item + "_" + field
}
