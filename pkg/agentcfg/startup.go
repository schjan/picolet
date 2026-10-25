package agentcfg

import (
	"errors"
	"fmt"
	"os"
)

// Locate returns the path the checking process reads cred at, for a
// rendered config whose credential paths are spelled as the Agent's next
// start sees them; an error refuses the config. A nil Locate reads every
// credential where it is spelled.
type Locate func(cred Credential) (string, error)

// located is a credential and the path the checks read it at.
type located struct {
	Credential

	at string
}

// shown names the credential's path for an error: the spelled path, and
// where it was read when that differs.
func (l located) shown() string {
	if l.at == l.Path {
		return l.Path
	}
	return l.Path + ", read at " + l.at
}

// onDisk is the credential at the path the checks read it at.
func (l located) onDisk() Credential {
	cred := l.Credential
	cred.Path = l.at
	return cred
}

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
	creds, err := c.locate(nil)
	if err != nil {
		return err
	}
	return c.checkStartup(creds)
}

func (c *Config) checkStartup(creds []located) error {
	if c.RepoURL == "" {
		return errors.New("repo_url is required for the run command")
	}
	for _, cred := range creds {
		if cred.Dir {
			continue
		}
		if _, err := os.ReadFile(cred.at); err != nil {
			return fmt.Errorf("credential %s (%s) is not readable: %w", cred.Setting, cred.shown(), err)
		}
		if cred.Setting == sopsIdentitySetting {
			if _, _, err := loadSopsIdentities(cred); err != nil {
				return err
			}
		}
	}
	return nil
}

// locate pairs each credential with the path locate gives it.
func (c *Config) locate(locate Locate) ([]located, error) {
	var creds []located
	for _, cred := range c.Credentials() {
		at := cred.Path
		if locate != nil {
			var err error
			if at, err = locate(cred); err != nil {
				return nil, fmt.Errorf("credential %s (%s): %w", cred.Setting, cred.Path, err)
			}
		}
		creds = append(creds, located{Credential: cred, at: at})
	}
	return creds, nil
}

// CheckRenderedConfig checks a rendered Agent config as the run command's
// startup would: it must parse strictly with this Agent's loader (an unknown
// key fails), pass Load's credential overlap check and pass CheckStartup,
// reading each credential file at the path locate gives it.
func CheckRenderedConfig(data []byte, locate Locate) error {
	cfg, err := Parse(data)
	if err != nil {
		return fmt.Errorf("parsing: %w", err)
	}
	creds, err := cfg.locate(locate)
	if err != nil {
		return err
	}
	if err := checkCredentialFiles(creds); err != nil {
		return err
	}
	return cfg.checkStartup(creds)
}
