package github

import (
	"context"
	"fmt"
	"net/http"
	"os"

	ghinstallation "github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gogithub "github.com/google/go-github/v88/github"
)

// Client wraps go-github with GitHub App auth.
type Client struct {
	gh        *gogithub.Client
	transport *ghinstallation.Transport
	Owner     string
	Repo      string
}

// NewClient creates a GitHub App client from a PEM key file.
func NewClient(appID, installationID int64, privateKeyPath, repoURL string) (*Client, error) {
	keyData, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading private key %s: %w", privateKeyPath, err)
	}
	return NewClientFromPEM(appID, installationID, keyData, repoURL)
}

// NewClientFromPEM creates a GitHub App client from PEM key data.
func NewClientFromPEM(appID, installationID int64, privateKeyPEM []byte, repoURL string) (*Client, error) {
	owner, repo, err := ParseRepoURL(repoURL)
	if err != nil {
		return nil, fmt.Errorf("parsing repo URL: %w", err)
	}

	itr, err := ghinstallation.New(http.DefaultTransport, appID, installationID, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("creating GitHub App transport: %w", err)
	}

	gh, err := gogithub.NewClient(gogithub.WithHTTPClient(&http.Client{Transport: itr}))
	if err != nil {
		return nil, fmt.Errorf("creating GitHub App client: %w", err)
	}

	return &Client{
		gh:        gh,
		transport: itr,
		Owner:     owner,
		Repo:      repo,
	}, nil
}

// newClientWithBaseURL is for testing — points the API client and transport at a test server.
func newClientWithBaseURL(
	appID, installationID int64,
	privateKeyPath, repoURL, baseURL string,
) (*Client, error) {
	owner, repo, err := ParseRepoURL(repoURL)
	if err != nil {
		return nil, fmt.Errorf("parsing repo URL: %w", err)
	}

	keyData, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading private key %s: %w", privateKeyPath, err)
	}

	itr, err := ghinstallation.New(http.DefaultTransport, appID, installationID, keyData)
	if err != nil {
		return nil, fmt.Errorf("creating GitHub App transport: %w", err)
	}

	itr.BaseURL = baseURL

	gh, err := gogithub.NewClient(gogithub.WithHTTPClient(&http.Client{Transport: itr}), gogithub.WithEnterpriseURLs(baseURL, baseURL))
	if err != nil {
		return nil, fmt.Errorf("configuring GitHub API URL: %w", err)
	}

	return &Client{
		gh:        gh,
		transport: itr,
		Owner:     owner,
		Repo:      repo,
	}, nil
}

// GitAuth returns a go-git transport.AuthMethod using a fresh GitHub App installation token.
func (c *Client) GitAuth(ctx context.Context) (transport.AuthMethod, error) {
	token, err := c.transport.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting installation token: %w", err)
	}

	return &githttp.BasicAuth{
		Username: "x-access-token",
		Password: token,
	}, nil
}
