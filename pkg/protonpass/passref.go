// Package protonpass integrates Picolet with the official Proton Pass CLI
// (https://protonpass.github.io/pass-cli/) for unattended secret resolution.
//
// References use the URI form pass://<share>/<item>/<field>, mirroring the
// scheme accepted by `pass-cli item view`. The CLI handles field extraction
// natively; Picolet only validates and forwards the URI.
package protonpass

import (
	"fmt"
	"strings"
)

// Prefix is the URI scheme prefix for Proton Pass secret references.
const Prefix = "pass://"

// IsRef reports whether s is a syntactically valid Proton Pass reference
// (pass://share/item/field).
func IsRef(s string) bool {
	_, err := ParseRef(s)
	return err == nil
}

// PassRef is a parsed pass:// secret reference.
type PassRef struct {
	Share string
	Item  string
	Field string
}

// ParseRef parses a Proton Pass secret reference of the form
// "pass://share/item/field". The field component may contain slashes
// (e.g. "pass://share/item/section/field").
func ParseRef(ref string) (PassRef, error) {
	path, ok := strings.CutPrefix(ref, Prefix)
	if !ok {
		return PassRef{}, fmt.Errorf("invalid pass:// reference %q: missing pass:// prefix", ref)
	}
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return PassRef{}, fmt.Errorf("invalid pass:// reference %q: expected pass://share/item/field", ref)
	}
	return PassRef{
		Share: parts[0],
		Item:  parts[1],
		Field: parts[2],
	}, nil
}

// PodmanSecretName returns the canonical Podman secret name derived from this reference.
// "pass://share/item/field" produces "share_item_field". Slashes in the field
// component are replaced with underscores.
func (r PassRef) PodmanSecretName() string {
	field := strings.ReplaceAll(r.Field, "/", "_")
	return r.Share + "_" + r.Item + "_" + field
}
