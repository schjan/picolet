package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
)

type fileReaderMode int

const (
	fileReaderStrict fileReaderMode = iota
	fileReaderPlaceholder
)

type resolveConfig struct {
	Hostname   string
	Service    string
	Rootless   bool
	DataDir    string
	SecretsDir string
	FileMode   fileReaderMode
}

// errHostnameRequired: the first input the bootstrap checks.
var errHostnameRequired = errors.New("hostname is required")

// openRepo opens the Fleet checked out at dir to resolve hostname from;
// close it when done. Missing inputs are reported in the bootstrap's
// order: hostname, then repo dir.
func openRepo(hostname, dir string) (*config.Repo, error) {
	if hostname == "" {
		return nil, errHostnameRequired
	}
	if dir == "" {
		return nil, errors.New("repo dir is required")
	}
	return config.OpenRepo(dir)
}

// resolveBootstrapHost resolves cfg.Service for cfg.Hostname from repo,
// which the caller opened and closes.
func resolveBootstrapHost(ctx context.Context, repo *config.Repo, cfg resolveConfig) (*resolver.ResolvedHost, error) {
	if cfg.Hostname == "" {
		return nil, errHostnameRequired
	}
	if cfg.Service == "" {
		return nil, fmt.Errorf("service is required")
	}

	readSecret, closeSecrets := secretReader(cfg.SecretsDir, cfg.FileMode)
	defer closeSecrets()
	r, err := resolver.New(resolver.Config{
		FS:           repo.FS,
		Config:       repo.Config,
		SecretReader: readSecret,
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

// secretReader returns the resolve pass's secret reader and the func that
// releases it.
func secretReader(secretsDir string, mode fileReaderMode) (resolver.SecretReader, func()) {
	if mode == fileReaderPlaceholder {
		return func(string) (string, error) { return "<secret>", nil }, func() {}
	}
	return resolver.DirSecretReader(secretsDir)
}
