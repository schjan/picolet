package github

import (
	"fmt"
	"net/url"
	"strings"
)

// ParseRepoURL extracts owner and repo from a GitHub HTTPS URL.
// SSH-style URLs are rejected because GitHub App tokens require HTTPS.
func ParseRepoURL(repoURL string) (owner, repo string, err error) {
	if repoURL == "" {
		return "", "", fmt.Errorf("empty repository URL")
	}

	if strings.HasPrefix(repoURL, "git@") || strings.HasPrefix(repoURL, "ssh://") || strings.HasPrefix(repoURL, "git+ssh://") {
		return "", "", fmt.Errorf("GitHub App requires HTTPS repository URL, got SSH: %s", repoURL)
	}

	u, err := url.Parse(repoURL)
	if err != nil {
		return "", "", fmt.Errorf("parsing URL %q: %w", repoURL, err)
	}
	if u.Scheme != "https" {
		return "", "", fmt.Errorf("GitHub App requires HTTPS repository URL, got scheme %q: %s", u.Scheme, repoURL)
	}

	if u.Hostname() != "github.com" {
		return "", "", fmt.Errorf("not a GitHub URL (host=%q): %s", u.Hostname(), repoURL)
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("expected owner/repo in URL path: %s", repoURL)
	}

	owner = parts[0]
	repo = strings.TrimSuffix(parts[1], ".git")

	return owner, repo, nil
}
