package bootstrap

import (
	"context"
	"fmt"
	"os"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
)

type fileReaderMode int

const (
	fileReaderStrict fileReaderMode = iota
	fileReaderPlaceholder
)

type resolveConfig struct {
	RepoDir string
	// Config is the already-loaded fleet config for RepoDir; loaded from
	// RepoDir when nil.
	Config     *config.Config
	Hostname   string
	Service    string
	Rootless   bool
	DataDir    string
	SecretsDir string
	FileMode   fileReaderMode
}

func resolveBootstrapHost(ctx context.Context, cfg resolveConfig) (*resolver.ResolvedHost, error) {
	if cfg.Hostname == "" {
		return nil, fmt.Errorf("hostname is required")
	}
	if cfg.RepoDir == "" {
		return nil, fmt.Errorf("repo dir is required")
	}
	if cfg.Service == "" {
		return nil, fmt.Errorf("service is required")
	}

	repoFS := os.DirFS(cfg.RepoDir)
	fleetCfg := cfg.Config
	if fleetCfg == nil {
		var err error
		fleetCfg, err = config.LoadAll(repoFS)
		if err != nil {
			return nil, fmt.Errorf("loading config: %w", err)
		}
	}
	r, err := resolver.New(resolver.Config{
		FS:           repoFS,
		Config:       fleetCfg,
		SecretReader: secretReader(cfg.SecretsDir, cfg.FileMode),
		Rootless:     cfg.Rootless,
		Strict:       true,
		DataDir:      cfg.DataDir,
	})
	if err != nil {
		return nil, fmt.Errorf("creating resolver: %w", err)
	}
	resolved, err := r.ResolveServicesForHost(ctx, cfg.Hostname, []string{cfg.Service})
	if err != nil {
		return nil, fmt.Errorf("resolving %s for %s: %w", cfg.Service, cfg.Hostname, err)
	}
	return resolved, nil
}

func secretReader(secretsDir string, mode fileReaderMode) resolver.SecretReader {
	if mode == fileReaderPlaceholder {
		return func(string) (string, error) { return "<secret>", nil }
	}
	// The Root is opened lazily on first use and reused across all secret
	// reads of the resolve pass; it stays open for the remainder of the
	// (short-lived) bootstrap process.
	var root *os.Root
	return func(path string) (string, error) {
		if root == nil {
			r, err := os.OpenRoot(secretsDir)
			if err != nil {
				return "", fmt.Errorf("opening secrets dir: %w", err)
			}
			root = r
		}
		data, err := root.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading secret %q: %w", path, err)
		}
		return string(data), nil
	}
}
