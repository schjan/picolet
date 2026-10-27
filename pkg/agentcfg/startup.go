package agentcfg

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/schjan/picolet/pkg/sops"
)

// Failures is the error of checks that report every failure they find
// (Load's credential overlap check, CheckStartup): one error per failure,
// each naming its setting and path. errors.Is and errors.As see each one.
type Failures []error

func (f Failures) Error() string { return errors.Join(f...).Error() }

func (f Failures) Unwrap() []error { return f }

// joinFailures is the Failures of the non-nil errs, nil when there are none.
func joinFailures(errs []error) error {
	var f Failures
	for _, err := range errs {
		if err != nil {
			f = append(f, err)
		}
	}
	if len(f) == 0 {
		return nil
	}
	return f
}

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

// Startup is what CheckStartup loaded, for the Agent to start with.
type Startup struct {
	// SopsIdentities are the sops block's age identities (LoadSopsIdentities);
	// nil without the block.
	SopsIdentities *sops.Identities
	// SopsIdentityLoose reports an identity file group or others may read.
	SopsIdentityLoose bool
}

// CheckStartup runs the checks the run command adds on top of Load: the
// config names a repo_url, and every credential file exists and is readable.
// They are file checks only and make no network calls (no 1Password
// handshake, no pass-cli login); the age identity file must also parse, and
// its identities are returned so the Agent does not read it again. A
// directory credential (protonpass.session_dir) is not required to exist:
// pass-cli creates it at its first login. Every failing check is reported
// (Failures), not only the first. Load has already rejected
// overlapping credentials, so CheckStartup does not repeat that check.
//
// The Agent runs them at startup and, through CheckRenderedConfig, before it
// applies a Fleet-rendered config of its own, so a commit cannot point the
// Agent at a config it would refuse to start with.
func (c *Config) CheckStartup() (Startup, error) {
	creds, err := c.locate(nil)
	if err != nil {
		return Startup{}, err
	}
	return c.checkStartup(creds)
}

func (c *Config) checkStartup(creds []located) (Startup, error) {
	var st Startup
	var errs []error
	if c.RepoURL == "" {
		errs = append(errs, errors.New("repo_url is required for the run command"))
	}
	for _, cred := range creds {
		var err error
		switch {
		case cred.Dir:
		case cred.Setting == sopsIdentitySetting:
			st.SopsIdentities, st.SopsIdentityLoose, err = loadSopsIdentities(cred)
		default:
			err = checkReadable(cred)
		}
		errs = append(errs, err)
	}
	if err := joinFailures(errs); err != nil {
		return Startup{}, err
	}
	return st, nil
}

// checkReadable fails when the file credential cannot be read, reading at
// most one byte of it: a directory fails as os.ReadFile would.
func checkReadable(cred located) error {
	f, err := os.Open(cred.at)
	if err != nil {
		return unreadable(cred, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Read(make([]byte, 1)); err != nil && !errors.Is(err, io.EOF) {
		return unreadable(cred, err)
	}
	return nil
}

// unreadable is the error of a credential file the checks cannot read.
func unreadable(cred located, err error) error {
	return fmt.Errorf("credential %s (%s) is not readable: %w", cred.Setting, cred.shown(), err)
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
	_, err = cfg.checkStartup(creds)
	return err
}
