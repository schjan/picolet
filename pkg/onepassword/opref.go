package onepassword

import "github.com/schjan/picolet/internal/secretref"

// Prefix is the URI scheme prefix for 1Password secret references.
const Prefix = "op://"

// TokenFileName is the service-account token's file name in a Host's
// secrets directory: where `bootstrap machine` places the Machine's token,
// and the token_path the reference Agent config reads.
const TokenFileName = "op-service-account-token" //nolint:gosec // G101: a file name, not a credential

// IsRef reports whether s is a valid 1Password secret reference (op://vault/item/field).
func IsRef(s string) bool {
	_, err := ParseOpRef(s)
	return err == nil
}

// OpRef is a parsed op:// secret reference (Scope is the vault).
type OpRef = secretref.Ref

// ParseOpRef parses a 1Password secret reference of the form "op://vault/item/field".
// The field component may contain slashes (e.g. "op://vault/item/section/field").
func ParseOpRef(ref string) (OpRef, error) {
	return secretref.Parse(Prefix, "vault", ref)
}
