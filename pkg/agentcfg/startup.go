package agentcfg

import (
	"fmt"
	"os"
)

// CheckStartup runs the Agent's startup credential checks: no two settings
// name the same credential, and every credential file exists and is
// readable. They are file checks only and make no network calls (no
// 1Password handshake, no pass-cli login). A directory credential
// (protonpass.session_dir) is not required to exist: pass-cli creates it at
// its first login.
//
// The Agent runs them at startup and, through CheckRendered, before it
// applies a Fleet-rendered config of its own, so a commit cannot point the
// Agent at a config it would refuse to start with.
func (c *Config) CheckStartup() error {
	if err := c.checkCredentialFiles(); err != nil {
		return err
	}
	for _, cred := range c.Credentials() {
		if cred.Dir {
			continue
		}
		if _, err := os.ReadFile(cred.Path); err != nil {
			return fmt.Errorf("credential %s (%s) is not readable: %w", cred.Setting, cred.Path, err)
		}
	}
	return nil
}

// CheckRendered checks a rendered Agent config as the Agent's startup would:
// it must parse strictly with this Agent's loader (an unknown key fails) and
// pass CheckStartup.
func CheckRendered(data []byte) error {
	cfg, err := Parse(data)
	if err != nil {
		return fmt.Errorf("parsing: %w", err)
	}
	return cfg.CheckStartup()
}
