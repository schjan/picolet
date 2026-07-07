package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/schjan/picolet/pkg/gitpoll"
	"github.com/schjan/picolet/pkg/resolver"
)

func (a *Agent) resolvePollerAuth(ctx context.Context) (gitpoll.AuthProvider, error) {
	if a.authProvider != nil {
		return a.authProvider, nil
	}

	// Mutual exclusion is enforced by agentcfg.Validate, so at most one
	// provider's git_token_ref is set; the file-based path is also excluded.
	// NOTE: the git token is resolved once at startup. If the underlying
	// secret rotates, a picolet restart is required — the per-tick refresh
	// cycle re-fetches assignment secrets but does NOT refresh the git token.
	if auth, err := a.gitAuthFromSecretProvider(ctx); auth != nil || err != nil {
		return auth, err
	}
	if gitpoll.IsSSHURL(a.cfg.RepoURL) {
		return gitpoll.NewSSHAgentAuth(a.cfg.RepoURL), nil
	}
	return gitpoll.NewTokenFileAuth(a.cfg.GitTokenPath), nil
}

// gitAuthFromSecretProvider returns a static-token auth provider when one
// of the configured secret providers supplies the git token ref. Returns
// (nil, nil) when no provider is configured for git auth, so the caller
// can fall through to SSH or file-based auth.
//
//nolint:nilnil // (nil, nil) signals "no provider auth configured"; caller handles fallback
func (a *Agent) gitAuthFromSecretProvider(ctx context.Context) (gitpoll.AuthProvider, error) {
	if a.opReader != nil && a.cfg.OnePassword != nil && a.cfg.OnePassword.GitTokenRef != "" {
		token, err := resolveGitToken(ctx, resolver.ProviderOnePassword, a.opReader, a.cfg.OnePassword.GitTokenRef)
		if err != nil {
			return nil, err
		}
		return gitpoll.NewStaticTokenAuth(a.cfg.RepoURL, token), nil
	}
	if a.ppReader != nil && a.cfg.ProtonPass != nil && a.cfg.ProtonPass.GitTokenRef != "" {
		token, err := resolveGitToken(ctx, resolver.ProviderProtonPass, a.ppReader, a.cfg.ProtonPass.GitTokenRef)
		if err != nil {
			return nil, err
		}
		return gitpoll.NewStaticTokenAuth(a.cfg.RepoURL, token), nil
	}
	return nil, nil
}

// resolveGitToken fetches a single ref via the given provider reader and
// validates that the response actually contains it.
func resolveGitToken(ctx context.Context, provider resolver.ProviderKey, reader resolver.SecretRefReader, ref string) (string, error) {
	results, err := reader(ctx, []string{ref})
	if err != nil {
		return "", fmt.Errorf("resolving git token from %s: %w", provider, err)
	}
	token, ok := results[ref]
	if !ok {
		return "", fmt.Errorf("resolving git token from %s: ref %q not in response", provider, ref)
	}
	slog.Info("git token resolved", "provider", string(provider))
	return token, nil
}

// refreshDue reports whether a provider's secrets should be re-fetched:
// never refreshed yet, or the refresh interval has elapsed.
func refreshDue(last time.Time, interval time.Duration) bool {
	return last.IsZero() || time.Since(last) >= interval
}

// opRefreshDue reports whether op:// secrets should be re-fetched.
// opReader is non-nil iff cfg.OnePassword is non-nil, so a single nil check suffices.
func (a *Agent) opRefreshDue() bool {
	return a.opReader != nil && refreshDue(a.lastOPRefresh, a.cfg.OnePassword.RefreshInterval)
}

// ppRefreshDue reports whether pass:// secrets should be re-fetched.
// ppReader is non-nil iff cfg.ProtonPass is non-nil, so a single nil check suffices.
func (a *Agent) ppRefreshDue() bool {
	return a.ppReader != nil && refreshDue(a.lastPPRefresh, a.cfg.ProtonPass.RefreshInterval)
}
