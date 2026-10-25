package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/schjan/picolet/pkg/agentcfg"
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
//
// A strict resolve reads --secrets-dir and renders twice: the first pass
// reads no secret file (placeholders) and yields the Agent config the bundle
// renders, the second reads the files, refusing that config's credentials
// as the Agent does (agentcfg's Config.RefuseCredentialAt). An Agent config
// the Fleet does not render names no credentials bootstrap can know.
func resolveBootstrapHost(ctx context.Context, repo *config.Repo, cfg resolveConfig) (*resolver.ResolvedHost, error) {
	if cfg.Hostname == "" {
		return nil, errHostnameRequired
	}
	if cfg.Service == "" {
		return nil, fmt.Errorf("service is required")
	}
	if cfg.FileMode == fileReaderPlaceholder {
		return resolveWith(ctx, repo, cfg, placeholderSecret)
	}
	placeholders, err := resolveWith(ctx, repo, cfg, placeholderSecret)
	if err != nil {
		return nil, err
	}
	guard, err := credentialGuard(placeholders.Files, cfg)
	if err != nil {
		return nil, err
	}
	readSecret, closeSecrets := resolver.DirSecretReader(cfg.SecretsDir, guard)
	defer closeSecrets()
	return resolveWith(ctx, repo, cfg, readSecret)
}

// credentialGuard refuses the credentials of the Agent config rendered in
// files (a pass with placeholder secrets), seen through cfg.SecretsDir; nil
// when the Fleet renders no Agent config.
func credentialGuard(files []resolver.ResolvedFile, cfg resolveConfig) (resolver.SecretGuard, error) {
	file, err := resolver.AgentConfigSecret(files, cfg.Service+".service")
	if errors.Is(err, resolver.ErrAgentConfigNotRendered) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	agentCfg, err := agentcfg.Parse([]byte(file.Content))
	if err != nil {
		return nil, fmt.Errorf("parsing rendered picolet config for its credentials: %w", err)
	}
	return agentCfg.RefuseCredentialAt(cfg.SecretsDir), nil
}

func resolveWith(ctx context.Context, repo *config.Repo, cfg resolveConfig, readSecret resolver.SecretReader) (*resolver.ResolvedHost, error) {
	// bootstrap never decrypts; strict mode fails every sops:// call.
	r, err := resolver.NewKeyless(resolver.Config{
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

// placeholderSecret stands in for every secret file: a pass that must not
// read --secrets-dir.
func placeholderSecret(string) (string, error) { return "<secret>", nil }
