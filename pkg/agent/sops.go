package agent

import (
	"errors"
	"fmt"
	"slices"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/sops"
)

// warnIdentityDrift logs a warning when the resolve decrypted SOPS files
// although none of the Agent's identities is among the Host's
// age_recipients:. The decrypts succeeded, so nothing fails; the warning
// makes the drift between host.yml and the identity file visible. Only
// public keys are logged.
func (a *Agent) warnIdentityDrift(host *config.HostConfig, reader *sops.Reader) {
	if host == nil || reader.DecryptedFiles() == 0 {
		return
	}
	own := a.sopsIDs.Recipients()
	for _, r := range own {
		if slices.Contains(host.AgeRecipients, r) {
			return
		}
	}
	a.logger.Warn("the Agent's age identities match none of its Host's age_recipients",
		"host", host.Hostname,
		"age_recipients", sops.RecipientStrings(host.AgeRecipients),
		"identity_recipients", sops.RecipientStrings(own),
		"fix", "add the identity's public key to age_recipients: in host.yml, or deliver the identity of a declared recipient")
}

// setupSops sets up the age identities named by the Agent config's sops
// block, the only identities the Agent decrypts sops:// refs with: those the
// startup checks loaded (WithStartup), else it loads them. A config without
// the block leaves the Agent without a SOPS reader, so a sops:// ref fails
// its resolve. A broken identity file stops the Agent, like a broken
// 1Password token; one readable by group or others only warns.
func (a *Agent) setupSops() error {
	var ids *sops.Identities
	var loose bool
	if a.startup != nil {
		ids, loose = a.startup.SopsIdentities, a.startup.SopsIdentityLoose
	} else {
		var err error
		if ids, loose, err = a.cfg.LoadSopsIdentities(); err != nil {
			return fmt.Errorf("setting up sops: %w", err)
		}
	}
	if ids == nil {
		return nil
	}
	path := a.cfg.Sops.AgeIdentityPath
	if loose {
		a.logger.Warn("age identity file is readable by group or others; restrict it to the Agent (chmod 600)",
			"path", path)
	}
	a.sopsIDs = ids
	a.logger.Info("sops identities loaded", "path", path, "identities", ids.Len())
	return nil
}

// scrubber removes decrypted SOPS values from text: a *sops.Reader or the
// sops.Scrubber built from one. scrubError's nil check does not catch a nil
// *sops.Reader in the interface; that works only because Reader.Scrub (via
// Reader.Scrubber) handles a nil receiver.
type scrubber interface {
	Scrub(text string) string
}

// scrubError returns err with every decrypted SOPS value of the resolve that
// s scrubs for removed from its text (sops.Reader.Scrub). Template helpers
// and validators may embed a rendered value in their errors, so every
// reconciliation error passes through here before it reaches a log line, a
// dashboard event, a deployment status or MQTT. An error whose text holds no
// such value, or that is already scrubbed, is returned as is.
func scrubError(s scrubber, err error) error {
	if err == nil || s == nil {
		return err
	}
	if _, done := err.(*scrubbedError); done { //nolint:errorlint // only an unwrapped scrubbedError holds a scrubbed text as a whole
		return err
	}
	msg := err.Error()
	clean := s.Scrub(msg)
	if clean == msg {
		return err
	}
	return &scrubbedError{msg: clean, err: err}
}

// scrubbedError carries a scrubbed message. It answers errors.Is for the
// original chain (the tick tells ErrApplyIncomplete and rollbacks apart that
// way) but does not unwrap, so no caller can reach the unscrubbed text, and
// errors.As finds nothing beneath it.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }

func (e *scrubbedError) Is(target error) bool { return errors.Is(e.err, target) }
