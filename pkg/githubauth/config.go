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
// or from secret-provider references (1Password or Proton Pass).
//
// Mutual exclusion between modes is enforced upstream by agentcfg.Validate;
// this function chooses the first configured source it finds.
func NewClientFromConfig(
	ctx context.Context,
	cfg *agentcfg.Config,
	opReader resolver.SecretRefReader,
	ppReader resolver.SecretRefReader,
) (*github.Client, int64, error) {
	if cfg == nil {
		return nil, 0, errors.New("agent config is required")
	}

	if cfg.HasGitHubAppRefs() {
		return newClientFromRefs(ctx, cfg, opReader, "onepassword", refTriple{
			AppID:          cfg.OnePassword.GitHubAppIDRef,
			InstallationID: cfg.OnePassword.GitHubInstallationRef,
			PrivateKey:     cfg.OnePassword.GitHubPrivateKeyRef,
		})
	}
	if cfg.HasGitHubAppPPRefs() {
		return newClientFromRefs(ctx, cfg, ppReader, "protonpass", refTriple{
			AppID:          cfg.ProtonPass.GitHubAppIDRef,
			InstallationID: cfg.ProtonPass.GitHubInstallationRef,
			PrivateKey:     cfg.ProtonPass.GitHubPrivateKeyRef,
		})
	}

	client, err := github.NewClient(cfg.GitHubAppID, cfg.GitHubInstallationID, cfg.GitHubPrivateKeyPath, cfg.RepoURL)
	if err != nil {
		return nil, 0, err
	}
	return client, cfg.GitHubAppID, nil
}

// refTriple groups the three refs needed for GitHub App authentication so
// they can be passed to a generic resolver.
type refTriple struct {
	AppID          string
	InstallationID string
	PrivateKey     string
}

func newClientFromRefs(ctx context.Context, cfg *agentcfg.Config, reader resolver.SecretRefReader, provider string, refs refTriple) (*github.Client, int64, error) {
	appID, installationID, privateKeyPEM, err := resolveGitHubAppFromRefs(ctx, reader, provider, refs)
	if err != nil {
		return nil, 0, err
	}
	client, err := github.NewClientFromPEM(appID, installationID, privateKeyPEM, cfg.RepoURL)
	if err != nil {
		return nil, 0, err
	}
	return client, appID, nil
}

func resolveGitHubAppFromRefs(
	ctx context.Context,
	reader resolver.SecretRefReader,
	provider string,
	refs refTriple,
) (int64, int64, []byte, error) {
	appIDRaw, installationRaw, privateKeyRaw, err := resolveGitHubAppRefValues(ctx, reader, provider, refs)
	if err != nil {
		return 0, 0, nil, err
	}
	return parseGitHubAppRefValues(appIDRaw, installationRaw, privateKeyRaw, provider)
}

func resolveGitHubAppRefValues(
	ctx context.Context,
	reader resolver.SecretRefReader,
	provider string,
	refs refTriple,
) (string, string, string, error) {
	if reader == nil {
		return "", "", "", fmt.Errorf("%s must be configured to resolve github app refs", provider)
	}

	results, err := reader(ctx, []string{refs.AppID, refs.InstallationID, refs.PrivateKey})
	if err != nil {
		return "", "", "", fmt.Errorf("resolving github app refs from %s: %w", provider, err)
	}

	appIDRaw, err := resolvedRef(results, refs.AppID, provider+".github_app_id_ref")
	if err != nil {
		return "", "", "", err
	}
	installationRaw, err := resolvedRef(results, refs.InstallationID, provider+".github_installation_id_ref")
	if err != nil {
		return "", "", "", err
	}
	privateKeyRaw, err := resolvedRef(results, refs.PrivateKey, provider+".github_private_key_ref")
	if err != nil {
		return "", "", "", err
	}
	return appIDRaw, installationRaw, privateKeyRaw, nil
}

func parseGitHubAppRefValues(appIDRaw, installationRaw, privateKeyRaw, provider string) (int64, int64, []byte, error) {
	appID, err := parsePositiveInt64(provider+".github_app_id_ref", appIDRaw)
	if err != nil {
		return 0, 0, nil, err
	}
	installationID, err := parsePositiveInt64(provider+".github_installation_id_ref", installationRaw)
	if err != nil {
		return 0, 0, nil, err
	}

	privateKey := strings.TrimSpace(privateKeyRaw)
	if privateKey == "" {
		return 0, 0, nil, fmt.Errorf("%s.github_private_key_ref resolved to an empty value", provider)
	}

	return appID, installationID, []byte(privateKey), nil
}

func resolvedRef(results map[string]string, ref, fieldName string) (string, error) {
	value, ok := results[ref]
	if !ok {
		return "", fmt.Errorf("resolving github app refs: missing %s (%q)", fieldName, ref)
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
