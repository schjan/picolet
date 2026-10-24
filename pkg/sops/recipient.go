package sops

import (
	"errors"
	"fmt"
	"strings"

	"filippo.io/age"
)

// RecipientType is the kind of an age recipient picolet accepts.
type RecipientType string

const (
	// RecipientX25519 is a native age X25519 recipient (age1…).
	RecipientX25519 RecipientType = "X25519"
	// RecipientHybrid is an age hybrid post-quantum recipient (age1pq1…).
	RecipientHybrid RecipientType = "hybrid"
)

// ErrUnsupportedRecipient is returned (wrapped) for a string that is not an
// X25519 or hybrid post-quantum age recipient.
var ErrUnsupportedRecipient = errors.New("unsupported age recipient")

// Recipient is a parsed age recipient in canonical form. Two Recipients are
// equal (==) exactly when they name the same public key, so comparisons never
// depend on how a label was spelled.
type Recipient struct {
	canonical string
	typ       RecipientType
}

// ParseRecipient parses an age public key. X25519 (age1…) and hybrid
// post-quantum (age1pq1…) recipients are accepted; SSH and plugin
// recipients are rejected. The input is not trimmed.
func ParseRecipient(s string) (Recipient, error) {
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(s, "ssh-"):
		return Recipient{}, fmt.Errorf("%w: SSH recipients are not supported, use an X25519 (age1…) or hybrid post-quantum (age1pq1…) key", ErrUnsupportedRecipient)
	case strings.HasPrefix(lower, "age1pq1"):
		r, err := age.ParseHybridRecipient(lower)
		if err != nil {
			return Recipient{}, notARecipient(s)
		}
		return Recipient{canonical: r.String(), typ: RecipientHybrid}, nil
	case strings.HasPrefix(lower, "age1") && strings.Contains(lower[len("age1"):], "1"):
		// Bech32 data never contains '1': a second separator means a plugin
		// HRP (age1<plugin>1…).
		return Recipient{}, fmt.Errorf("%w: plugin recipients are not supported, use an X25519 (age1…) or hybrid post-quantum (age1pq1…) key", ErrUnsupportedRecipient)
	}
	r, err := age.ParseX25519Recipient(lower)
	if err != nil {
		return Recipient{}, notARecipient(s)
	}
	return Recipient{canonical: r.String(), typ: RecipientX25519}, nil
}

func notARecipient(s string) error {
	if strings.HasPrefix(strings.ToUpper(s), "AGE-SECRET-KEY-") {
		// Never echo a private key.
		return fmt.Errorf("%w: an age identity (secret key) is not an age recipient; use its public key", ErrUnsupportedRecipient)
	}
	const maxShown = 24
	shown := s
	if len(shown) > maxShown {
		shown = shown[:maxShown] + "…"
	}
	return fmt.Errorf("%w: %q is not an age recipient (want age1… or age1pq1…)", ErrUnsupportedRecipient, shown)
}

// String returns the canonical (lowercase Bech32) form.
func (r Recipient) String() string { return r.canonical }

// Type returns the recipient's kind.
func (r Recipient) Type() RecipientType { return r.typ }

// MarshalText implements encoding.TextMarshaler with the canonical form.
func (r Recipient) MarshalText() ([]byte, error) { return []byte(r.canonical), nil }
