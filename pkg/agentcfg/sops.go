package agentcfg

import (
	"fmt"
	"io"
	"os"

	"github.com/schjan/picolet/pkg/sops"
)

const sopsIdentitySetting = "sops.age_identity_path"

// groupOrOtherReadable is the permission mask of the read bits for group and
// others.
const groupOrOtherReadable os.FileMode = 0o044

// LoadSopsIdentities reads and parses the age identity file named by
// sops.age_identity_path. It returns (nil, false, nil) when the config has
// no sops block. A file that is missing, unreadable or unparsable, or holds
// no identity, is an error that names the setting and the path but never
// quotes the file's content. groupOrWorldReadable reports a file mode that
// lets group or others read the identity; the Agent warns about it.
func (c *Config) LoadSopsIdentities() (ids *sops.Identities, groupOrWorldReadable bool, err error) {
	if c.Sops == nil {
		return nil, false, nil
	}
	path := c.Sops.AgeIdentityPath
	data, mode, err := readWithMode(path)
	if err != nil {
		return nil, false, fmt.Errorf("credential %s (%s) is not readable: %w", sopsIdentitySetting, path, err)
	}
	ids, err = sops.ParseIdentities(data)
	if err != nil {
		return nil, false, fmt.Errorf("credential %s (%s): %w", sopsIdentitySetting, path, err)
	}
	return ids, mode.Perm()&groupOrOtherReadable != 0, nil
}

// readWithMode reads the file at path and returns its content and the mode of
// the file it read, from one open.
func readWithMode(path string) ([]byte, os.FileMode, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}
	return data, info.Mode(), nil
}
