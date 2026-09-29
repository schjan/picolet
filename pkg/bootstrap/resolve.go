package bootstrap

import (
	"context"
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
	RepoDir    string
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

	repo, err := config.OpenRepo(cfg.RepoDir)
	if err != nil {
		return nil, err
	}
	defer repo.Close()
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
