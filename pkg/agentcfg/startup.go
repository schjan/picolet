package agentcfg

import (
	"errors"
	"fmt"
	"os"
)

// CheckStartup runs the checks the run command adds on top of Load: the
// config names a repo_url, and every credential file exists and is readable.
// They are file checks only and make no network calls (no 1Password
// handshake, no pass-cli login); the age identity file must also parse. A directory credential
// (protonpass.session_dir) is not required to exist: pass-cli creates it at
// its first login. Load has already rejected overlapping credentials, so
// CheckStartup does not repeat that check.
//
// The Agent runs them at startup and, through CheckRenderedConfig, before it
// applies a Fleet-rendered config of its own, so a commit cannot point the
// Agent at a config it would refuse to start with.
func (c *Config) CheckStartup() error {
	if c.RepoURL == "" {
		return errors.New("repo_url is required for the run command")
	}
	for _, cred := range c.Credentials() {
		if cred.Dir {
			continue
		}
		if _, err := os.ReadFile(cred.Path); err != nil {
			return fmt.Errorf("credential %s (%s) is not readable: %w", cred.Setting, cred.Path, err)
		}
	}
	if _, _, err := c.LoadSopsIdentities(); err != nil {
		return err
	}
	return nil
}

// CheckRenderedConfig checks a rendered Agent config as the run command's
// startup would: it must parse strictly with this Agent's loader (an unknown
// key fails), pass Load's credential overlap check and pass CheckStartup.
func CheckRenderedConfig(data []byte) error {
	cfg, err := Parse(data)
	if err != nil {
		return fmt.Errorf("parsing: %w", err)
	}
	if err := cfg.checkCredentialFiles(); err != nil {
		return err
	}
	return cfg.CheckStartup()
}
