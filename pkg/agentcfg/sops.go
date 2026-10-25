package agentcfg

import (
	"fmt"
	"os"

	"github.com/schjan/picolet/pkg/sops"
)

const sopsIdentitySetting = "sops.age_identity_path"

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
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("credential %s (%s) is not readable: %w", sopsIdentitySetting, path, err)
	}
	ids, err = sops.ParseIdentities(data)
	if err != nil {
		return nil, false, fmt.Errorf("credential %s (%s): %w", sopsIdentitySetting, path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, false, fmt.Errorf("credential %s (%s): %w", sopsIdentitySetting, path, err)
	}
	return ids, info.Mode().Perm()&0o044 != 0, nil
}
