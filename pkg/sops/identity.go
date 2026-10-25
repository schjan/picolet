package sops

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"filippo.io/age"
)

// NoIdentityHint is the fix appended ("; " + NoIdentityHint) to an
// ErrNoIdentity error for a sops:// ref read without identities.
const NoIdentityHint = "set sops.age_identity_path in the Agent config"

var (
	// ErrNoIdentity is returned (wrapped) when a sops:// ref is read without
	// any age identity configured, and by ParseIdentities for a file that
	// holds none.
	ErrNoIdentity = errors.New("no age identity configured")
	// ErrBadIdentity is returned (wrapped) by ParseIdentities for a line that
	// is not an X25519 or hybrid post-quantum age identity. The message names
	// the line number only, never its content.
	ErrBadIdentity = errors.New("invalid age identity")
)

// Identities is a parsed set of age identities (secret keys) and the
// recipients derived from them. Only X25519 and hybrid post-quantum
// identities are accepted.
type Identities struct {
	ids        []age.Identity
	recipients []Recipient
}

const (
	agePluginIdentityPrefix = "AGE-PLUGIN-"
	ageHybridIdentityPrefix = "AGE-SECRET-KEY-PQ-1"
)

// ParseIdentities parses an age identity file: one AGE-SECRET-KEY-… per
// line, blank lines and '#' comments ignored. Plugin and SSH identities are
// rejected; a file with no identity is ErrNoIdentity. Errors never quote the
// file's content.
func ParseIdentities(data []byte) (*Identities, error) {
	var set Identities
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := set.add(line); err != nil {
			return nil, fmt.Errorf("%w on line %d: %w", ErrBadIdentity, i+1, err)
		}
	}
	if len(set.ids) == 0 {
		return nil, fmt.Errorf("%w: the identity file holds no AGE-SECRET-KEY-… line", ErrNoIdentity)
	}
	return &set, nil
}

// add parses one identity line and adds the identity and its recipient.
func (s *Identities) add(line string) error {
	upper := strings.ToUpper(line)
	switch {
	case strings.HasPrefix(upper, agePluginIdentityPrefix):
		return errors.New("plugin identities are not supported, use an X25519 or hybrid post-quantum age key")
	case strings.HasPrefix(line, "-----BEGIN"), strings.HasPrefix(line, "ssh-"):
		return errors.New("SSH keys are not supported, use an X25519 or hybrid post-quantum age key")
	case strings.HasPrefix(upper, ageHybridIdentityPrefix):
		id, err := age.ParseHybridIdentity(upper)
		if err != nil {
			return errors.New("malformed hybrid post-quantum age identity")
		}
		s.addIdentity(id, Recipient{canonical: id.Recipient().String(), typ: RecipientHybrid})
		return nil
	case strings.HasPrefix(upper, ageIdentityPrefix):
		id, err := age.ParseX25519Identity(upper)
		if err != nil {
			return errors.New("malformed X25519 age identity")
		}
		s.addIdentity(id, Recipient{canonical: id.Recipient().String(), typ: RecipientX25519})
		return nil
	}
	return errors.New("not an age identity (want AGE-SECRET-KEY-…)")
}

func (s *Identities) addIdentity(id age.Identity, r Recipient) {
	s.ids = append(s.ids, id)
	if !slices.Contains(s.recipients, r) {
		s.recipients = append(s.recipients, r)
	}
}

// Recipients returns the recipients of the identities, in file order without
// duplicates.
func (s *Identities) Recipients() []Recipient {
	if s == nil {
		return nil
	}
	return append([]Recipient(nil), s.recipients...)
}

// Len returns the number of identities.
func (s *Identities) Len() int {
	if s == nil {
		return 0
	}
	return len(s.ids)
}
