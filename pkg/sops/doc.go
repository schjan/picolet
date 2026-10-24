// Package sops owns everything picolet does with SOPS-encrypted files:
// decryption with explicit age identities, keyless inspection, name rules
// and recipient parsing (spec #248). It is the only package allowed to
// import github.com/getsops/sops/v3; a depguard rule in .golangci.yml
// enforces that.
//
// This file is a placeholder: the blank import makes the dependency real
// before any SOPS code is written.
package sops

import (
	_ "github.com/getsops/sops/v3" // placeholder until the SOPS package is implemented
)
