package agent

import (
	"errors"
	"fmt"

	"github.com/schjan/picolet/pkg/sops"
)

// setupSops loads the age identities named by the Agent config's sops block,
// the only identities the Agent decrypts sops:// refs with. A config without
// the block leaves the Agent without a SOPS reader, so a sops:// ref fails
// its resolve. A broken identity file stops the Agent, like a broken
// 1Password token; one readable by group or others only warns.
func (a *Agent) setupSops() error {
	ids, loose, err := a.cfg.LoadSopsIdentities()
	if err != nil {
		return fmt.Errorf("setting up sops: %w", err)
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

// scrubError returns err with every decrypted SOPS value of the resolve that
// reader served removed from its text (sops.Reader.Scrub). Template helpers
// and validators may embed a rendered value in their errors, so every
// reconciliation error passes through here before it reaches a log line, a
// dashboard event, a deployment status or MQTT. An error whose text holds no
// such value is returned as is.
func scrubError(reader *sops.Reader, err error) error {
	if err == nil || reader == nil {
		return err
	}
	msg := err.Error()
	clean := reader.Scrub(msg)
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
