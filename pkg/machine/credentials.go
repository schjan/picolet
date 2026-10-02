package machine

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
)

// credentialMode is the permission of every credential file placed.
const credentialMode fs.FileMode = 0o600

// credentialSteps plans a step per file below <hostname>/ of src, in lexical
// order, each placing the file at the same relative path in h's secrets
// directory. A Host without a directory in src gets a warning instead.
func credentialSteps(h Host, src *SecretsDir) (steps []Step, warning string, err error) {
	if src == nil {
		return nil, "", nil
	}
	hostDir := filepath.Join(src.Path, h.Hostname)
	info, err := fs.Stat(src.FS, h.Hostname)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Sprintf("no credential files for %s: %s does not exist", h.Hostname, hostDir), nil
	case err != nil:
		return nil, "", fmt.Errorf("reading --secrets-dir: %w", err)
	case !info.IsDir():
		return nil, "", fmt.Errorf("reading --secrets-dir: %s is not a directory", hostDir)
	}
	secrets := secretsAgentDir.path(h)
	err = fs.WalkDir(src.FS, h.Hostname, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := readCredential(src.FS, name)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Join(src.Path, filepath.FromSlash(name)), err)
		}
		rel := strings.TrimPrefix(name, h.Hostname+"/")
		steps = append(steps, Step{
			ID:      h.Hostname + "/credential/" + rel,
			Phase:   PhaseCredentials,
			Kind:    StepCredential,
			Host:    h,
			Path:    path.Join(secrets, rel),
			Mode:    credentialMode,
			Content: content,
		})
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("reading --secrets-dir: %w", err)
	}
	return steps, "", nil
}

// readCredential reads the regular file name, following a symlink that
// stays within fsys; anything else is an error.
func readCredential(fsys fs.FS, name string) ([]byte, error) {
	info, err := fs.Stat(fsys, name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return fs.ReadFile(fsys, name)
}
