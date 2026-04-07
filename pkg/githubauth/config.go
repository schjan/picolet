package githubauth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/github"
	"github.com/schjan/picolet/pkg/resolver"
)

// NewClientFromConfig creates a GitHub App client from direct config fields
// or from 1Password references.
func NewClientFromConfig(
	ctx context.Context,
	cfg *agentcfg.Config,
	opReader resolver.OpSecretReader,
) (*github.Client, int64, error) {
	if cfg == nil {
		return nil, 0, errors.New("agent config is required")
	}

	if cfg.HasGitHubAppRefs() {
		appID, installationID, privateKeyPEM, err := resolveGitHubAppFromOnePassword(ctx, cfg, opReader)
		if err != nil {
			return nil, 0, err
		}
		client, err := github.NewClientFromPEM(appID, installationID, privateKeyPEM, cfg.RepoURL)
		if err != nil {
			return nil, 0, err
		}
		return client, appID, nil
	}

	client, err := github.NewClient(cfg.GitHubAppID, cfg.GitHubInstallationID, cfg.GitHubPrivateKeyPath, cfg.RepoURL)
	if err != nil {
		return nil, 0, err
	}
	return client, cfg.GitHubAppID, nil
}

func resolveGitHubAppFromOnePassword(
	ctx context.Context,
	cfg *agentcfg.Config,
	opReader resolver.OpSecretReader,
) (int64, int64, []byte, error) {
	appIDRaw, installationRaw, privateKeyRaw, err := resolveGitHubAppRefValues(ctx, cfg, opReader)
	if err != nil {
		return 0, 0, nil, err
	}
	return parseGitHubAppRefValues(appIDRaw, installationRaw, privateKeyRaw)
}

func resolveGitHubAppRefValues(
	ctx context.Context,
	cfg *agentcfg.Config,
	opReader resolver.OpSecretReader,
) (string, string, string, error) {
	if opReader == nil || cfg.OnePassword == nil {
		return "", "", "", errors.New("onepassword must be configured to resolve github app refs")
	}

	refs := []string{
		cfg.OnePassword.GitHubAppIDRef,
		cfg.OnePassword.GitHubInstallationRef,
		cfg.OnePassword.GitHubPrivateKeyRef,
	}
	results, err := opReader(ctx, refs)
	if err != nil {
		return "", "", "", fmt.Errorf("resolving github app refs from 1password: %w", err)
	}

	appIDRaw, err := resolvedRef(results, cfg.OnePassword.GitHubAppIDRef, "onepassword.github_app_id_ref")
	if err != nil {
		return "", "", "", err
	}
	installationRaw, err := resolvedRef(results, cfg.OnePassword.GitHubInstallationRef, "onepassword.github_installation_id_ref")
	if err != nil {
		return "", "", "", err
	}
	privateKeyRaw, err := resolvedRef(results, cfg.OnePassword.GitHubPrivateKeyRef, "onepassword.github_private_key_ref")
	if err != nil {
		return "", "", "", err
	}
	return appIDRaw, installationRaw, privateKeyRaw, nil
}

func parseGitHubAppRefValues(appIDRaw, installationRaw, privateKeyRaw string) (int64, int64, []byte, error) {
	appID, err := parsePositiveInt64("onepassword.github_app_id_ref", appIDRaw)
	if err != nil {
		return 0, 0, nil, err
	}
	installationID, err := parsePositiveInt64("onepassword.github_installation_id_ref", installationRaw)
	if err != nil {
		return 0, 0, nil, err
	}

	privateKey := strings.TrimSpace(privateKeyRaw)
	if privateKey == "" {
		return 0, 0, nil, errors.New("onepassword.github_private_key_ref resolved to an empty value")
	}

	return appID, installationID, []byte(privateKey), nil
}

func resolvedRef(results map[string]string, ref, fieldName string) (string, error) {
	value, ok := results[ref]
	if !ok {
		return "", fmt.Errorf("resolving github app refs from 1password: missing %s (%q)", fieldName, ref)
	}
	return value, nil
}

func parsePositiveInt64(name, value string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid integer: %w", name, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return n, nil
}
