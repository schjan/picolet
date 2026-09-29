package config

import (
	"fmt"
	"io/fs"
	"os"
)

// Repo is a Fleet repository opened for reading. FS is confined to the
// repository directory: unlike os.DirFS, a symlink cannot reach host files
// outside it (the agent runs as root and deploys what the repo resolves to).
type Repo struct {
	FS     fs.FS
	Config *Config
	root   *os.Root
}

// OpenRepo opens dir as a Fleet repository and loads its config. FS is valid
// until Close.
func OpenRepo(dir string, opts ...LoadOption) (*Repo, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening fleet repo: %w", err)
	}
	cfg, err := LoadAll(root.FS(), opts...)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("loading config: %w", err)
	}
	return &Repo{FS: root.FS(), Config: cfg, root: root}, nil
}

// Close releases the repository directory.
func (r *Repo) Close() error {
	return r.root.Close()
}
