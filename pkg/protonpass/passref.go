// Package protonpass integrates Picolet with the official Proton Pass CLI
// (https://protonpass.github.io/pass-cli/) for unattended secret resolution.
//
// References use the URI form pass://<share>/<item>/<field>, mirroring the
// scheme accepted by `pass-cli item view`. The CLI handles field extraction
// natively; Picolet only validates and forwards the URI.
package protonpass

import "github.com/schjan/picolet/internal/secretref"

// Prefix is the URI scheme prefix for Proton Pass secret references.
const Prefix = "pass://"

// PATFileName is the personal access token's file name in a Host's secrets
// directory: where `bootstrap machine` places the Machine's PAT, and the
// pat_path the reference Agent config reads.
const PATFileName = "pp-pat"

// IsRef reports whether s is a syntactically valid Proton Pass reference
// (pass://share/item/field).
func IsRef(s string) bool {
	_, err := ParseRef(s)
	return err == nil
}

// PassRef is a parsed pass:// secret reference: an alias of the parser
// shared with 1Password, whose Scope field is the share (the field was Share
// before the parsers were shared). No code outside this module uses it.
type PassRef = secretref.Ref

// ParseRef parses a Proton Pass secret reference of the form
// "pass://share/item/field". The field component may contain slashes
// (e.g. "pass://share/item/section/field").
func ParseRef(ref string) (PassRef, error) {
	return secretref.Parse(Prefix, "share", ref)
}
