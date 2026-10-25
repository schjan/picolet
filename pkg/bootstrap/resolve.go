package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

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
// as the Agent does (bootstrapCredentials). The second pass's Agent config,
// the one deployed, must name the same credentials: settings that depend on
// a secret file would escape the guard.
func resolveBootstrapHost(ctx context.Context, repo *config.Repo, cfg resolveConfig) (*resolver.ResolvedHost, error) {
	if cfg.Hostname == "" {
		return nil, errHostnameRequired
	}
	if cfg.Service == "" {
		return nil, fmt.Errorf("service is required")
	}
	placeholders, err := resolveWith(ctx, repo, cfg, placeholderSecret)
	if err != nil || cfg.FileMode == fileReaderPlaceholder {
		return placeholders, err
	}
	creds, err := bootstrapCredentials(placeholders.Files, cfg)
	if err != nil {
		return nil, err
	}
	readSecret, closeSecrets := resolver.DirSecretReader(cfg.SecretsDir, agentcfg.RefuseCredentials(creds))
	defer closeSecrets()
	resolved, err := resolveWith(ctx, repo, cfg, readSecret)
	if err != nil {
		return nil, err
	}
	deployed, err := bootstrapCredentials(resolved.Files, cfg)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(creds, deployed) {
		return nil, errors.New("the rendered picolet config names other credentials once secret files are read: its credential settings must not depend on a secret file")
	}
	return resolved, nil
}

// conventionalIdentity is the --secrets-dir file the age identity is
// delivered as (README, sops.age_identity_path).
const conventionalIdentity = "age-identity"

// bootstrapCredentials lists the files a bootstrap resolve refuses to read:
// the credentials of the Agent config rendered in files, seen through
// cfg.SecretsDir (agentcfg's Config.CredentialsAt), and always the
// conventional identity file below cfg.SecretsDir, which also covers an
// Agent config the Fleet does not render (its credentials are unknown).
func bootstrapCredentials(files []resolver.ResolvedFile, cfg resolveConfig) ([]agentcfg.Credential, error) {
	var creds []agentcfg.Credential
	file, err := resolver.AgentConfigSecret(files, cfg.Service+".service")
	switch {
	case errors.Is(err, resolver.ErrAgentConfigNotRendered):
	case err != nil:
		return nil, err
	default:
		agentCfg, err := agentcfg.Parse([]byte(file.Content))
		if err != nil {
			return nil, fmt.Errorf("parsing rendered picolet config for its credentials: %w", err)
		}
		creds = agentCfg.CredentialsAt(cfg.SecretsDir)
	}
	// Last, so a refusal names the config's own setting when it has one.
	return append(creds, agentcfg.Credential{
		Setting: "conventional sops.age_identity_path",
		Path:    filepath.Join(cfg.SecretsDir, conventionalIdentity),
	}), nil
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
