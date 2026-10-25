package agentcfg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	pp "github.com/schjan/picolet/pkg/protonpass"
)

// Credential is a file the Agent authenticates with, named by an Agent
// config setting. A Fleet must never deploy one (Config.RefuseCredential),
// and no two settings may name the same one (checked by Load).
type Credential struct {
	Setting string // config key, e.g. "git_token_path"
	Path    string
	Dir     bool // a directory: every file under it is a credential
}

// Credentials lists every file this config uses as a credential, explicitly
// by setting; settings that only locate tools (cli_path) are not listed.
// Unset settings are left out.
func (c *Config) Credentials() []Credential {
	var creds []Credential
	add := func(setting, path string, dir bool) {
		if path != "" {
			creds = append(creds, Credential{Setting: setting, Path: path, Dir: dir})
		}
	}
	if c.OnePassword != nil {
		add("onepassword.token_path", c.OnePassword.TokenPath, false)
	}
	if c.ProtonPass != nil {
		add("protonpass.pat_path", c.ProtonPass.PATPath, false)
		add("protonpass.session_dir", protonPassSessionDir(c.ProtonPass), true)
	}
	add("git_token_path", c.GitTokenPath, false)
	add("github_private_key_path", c.GitHubPrivateKeyPath, false)
	add("webhook_secret_path", c.WebhookSecretPath, false)
	if c.MQTT != nil {
		add("mqtt.password_path", c.MQTT.PasswordPath, false)
	}
	return creds
}

// protonPassSessionDir is the session dir pass-cli is pointed at: the
// configured one, or the PAT-mode default. Lazy mode (no PAT, no
// session_dir) leaves pass-cli's own default, which picolet does not know.
func protonPassSessionDir(c *ProtonPassConfig) string {
	if c.SessionDir != "" {
		return c.SessionDir
	}
	if c.PATPath != "" {
		return pp.DefaultSessionDir
	}
	return ""
}

// RefuseCredential returns an error when info (the file a secret read
// opened) is one of the Agent's credentials, compared with os.SameFile so
// symlinks, hard links and other spellings are caught. It is the Agent's
// resolver.SecretGuard.
func (c *Config) RefuseCredential(info fs.FileInfo) error {
	for _, cred := range c.Credentials() {
		held, err := cred.holds(info)
		if err != nil {
			return fmt.Errorf("checking Agent credential %s (%s): %w", cred.Setting, cred.Path, err)
		}
		if held {
			return fmt.Errorf("it is the Agent credential %s (%s): Agent credentials cannot be deployed", cred.Setting, cred.Path)
		}
	}
	return nil
}

// holds reports whether info is the credential file or, for a Dir
// credential, the directory or a file anywhere under it. A credential not on
// disk holds nothing.
func (cred Credential) holds(info fs.FileInfo) (bool, error) {
	onDisk, err := os.Stat(cred.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if os.SameFile(info, onDisk) {
		return true, nil
	}
	if !cred.Dir {
		return false, nil
	}
	return dirHolds(cred.Path, info)
}

// dirHolds reports whether info is a file anywhere under dir. The caller
// opened info's file before this walk, so a file under dir is there to be
// found; entries removed during the walk cannot be it.
func dirHolds(dir string, info fs.FileInfo) (bool, error) {
	// WalkDir does not descend into a symlinked root, so resolve it first.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false, err
	}
	errFound := errors.New("found")
	err = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		entry, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if os.SameFile(info, entry) {
			return errFound
		}
		return nil
	})
	if errors.Is(err, errFound) {
		return true, nil
	}
	return false, err
}

// checkCredentialFiles fails when two credential settings name the same
// file, or one names a file inside the other's directory: a credential the
// Agent sends somewhere (git_token_path, mqtt.password_path, …) pointed at
// another credential would send that one instead. It reads the filesystem,
// so Load and CheckRenderedConfig run it and Parse does not.
func (c *Config) checkCredentialFiles() error {
	creds := c.Credentials()
	for i, a := range creds {
		for _, b := range creds[i+1:] {
			same, err := a.overlaps(b)
			if err != nil {
				return err
			}
			if same {
				return fmt.Errorf("%s (%s) and %s (%s) name the same credential file", a.Setting, a.Path, b.Setting, b.Path)
			}
		}
	}
	return nil
}

// overlaps reports whether cred and other name the same file, or one names
// a file under the other's directory: by spelling, so files not yet on disk
// count, and by os.SameFile on what is there.
func (cred Credential) overlaps(other Credential) (bool, error) {
	if filepath.Clean(cred.Path) == filepath.Clean(other.Path) || cred.spellsUnder(other.Path) || other.spellsUnder(cred.Path) {
		return true, nil
	}
	for _, pair := range [][2]Credential{{cred, other}, {other, cred}} {
		file, holder := pair[0], pair[1]
		info, err := os.Stat(file.Path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // not on disk: nothing to compare
		}
		if err != nil {
			return false, fmt.Errorf("checking %s (%s): %w", file.Setting, file.Path, err)
		}
		held, err := holder.holds(info)
		if err != nil {
			return false, fmt.Errorf("checking %s (%s): %w", holder.Setting, holder.Path, err)
		}
		if held {
			return true, nil
		}
	}
	return false, nil
}

// spellsUnder reports whether path spells a path under cred's directory; a
// file credential has nothing under it.
func (cred Credential) spellsUnder(path string) bool {
	if !cred.Dir {
		return false
	}
	return strings.HasPrefix(filepath.Clean(path), filepath.Clean(cred.Path)+string(filepath.Separator))
}
