package gitpoll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// AuthProvider provides authentication for git operations.
type AuthProvider interface {
	GitAuth(ctx context.Context) (transport.AuthMethod, error)
}

type noAuthProvider struct{}

//nolint:nilnil // nil signals anonymous access
func (noAuthProvider) GitAuth(_ context.Context) (transport.AuthMethod, error) {
	return nil, nil
}

type tokenFileAuth struct {
	path string
}

type tokenValueAuth struct {
	token string
}

//nolint:nilnil // nil signals anonymous access; interface needed for auth abstraction
func (a *tokenFileAuth) GitAuth(_ context.Context) (transport.AuthMethod, error) {
	if a.path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		return nil, fmt.Errorf("reading token from %s: %w", a.path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, nil
	}
	return &http.BasicAuth{Username: "x", Password: token}, nil
}

//nolint:nilnil // nil signals anonymous access; interface needed for auth abstraction
func (a *tokenValueAuth) GitAuth(_ context.Context) (transport.AuthMethod, error) {
	token := strings.TrimSpace(a.token)
	if token == "" {
		return nil, nil
	}
	return &http.BasicAuth{Username: "x", Password: token}, nil
}

type sshAgentAuth struct {
	repoURL string
}

func (a *sshAgentAuth) GitAuth(_ context.Context) (transport.AuthMethod, error) {
	user := "git"
	if ep, err := transport.NewEndpoint(a.repoURL); err == nil && ep.User != "" {
		user = ep.User
	}
	auth, err := ssh.NewSSHAgentAuth(user)
	if err != nil {
		return nil, fmt.Errorf("SSH agent auth: %w", err)
	}
	return auth, nil
}

// NewTokenFileAuth creates an AuthProvider that reads a PAT from a file.
func NewTokenFileAuth(tokenPath string) AuthProvider {
	return &tokenFileAuth{path: tokenPath}
}

// NewSSHAgentAuth creates an AuthProvider using the SSH agent.
func NewSSHAgentAuth(repoURL string) AuthProvider {
	return &sshAgentAuth{repoURL: repoURL}
}

// IsSSHURL reports whether the given URL uses an SSH transport.
func IsSSHURL(url string) bool {
	if strings.HasPrefix(url, "git+ssh://") {
		return true
	}
	endpoint, err := transport.NewEndpoint(url)
	if err != nil {
		return false
	}
	return endpoint.Protocol == "ssh" || endpoint.Protocol == "git+ssh"
}

// Poller manages a local clone of a remote git repo and polls for changes.
type Poller struct {
	repoURL   string
	branch    string
	localPath string
	auth      AuthProvider
	repo      *git.Repository
}

// PollResult indicates the current HEAD SHA and whether it changed.
type PollResult struct {
	HeadSHA string
	Changed bool
}

// New creates a poller. localPath is where the repo is cloned to.
func New(repoURL, branch, localPath string, auth AuthProvider) *Poller {
	if auth == nil {
		auth = noAuthProvider{}
	}
	return &Poller{
		repoURL:   repoURL,
		branch:    branch,
		localPath: localPath,
		auth:      auth,
	}
}

// NewWithToken creates a poller that uses a direct token value for HTTP authentication.
// The token takes precedence over a token file path.
func NewWithToken(repoURL, branch, localPath, token string) *Poller {
	return New(repoURL, branch, localPath, NewStaticTokenAuth(repoURL, token))
}

// Init opens an existing clone or clones fresh.
func (p *Poller) Init(ctx context.Context) error {
	repo, err := git.PlainOpen(p.localPath)
	if err == nil && p.verifyRemote(repo) {
		p.repo = repo
		return p.fetch(ctx)
	}
	if err == nil {
		// Existing clone has wrong remote — remove and re-clone.
		if err := os.RemoveAll(p.localPath); err != nil {
			return fmt.Errorf("removing stale clone: %w", err)
		}
	}

	auth, err := p.auth.GitAuth(ctx)
	if err != nil {
		return err
	}

	repo, err = git.PlainCloneContext(ctx, p.localPath, false, &git.CloneOptions{
		URL:           p.repoURL,
		ReferenceName: plumbing.NewBranchReferenceName(p.branch),
		SingleBranch:  true,
		Auth:          auth,
	})
	if err != nil {
		return fmt.Errorf("cloning %s: %w", p.repoURL, err)
	}
	p.repo = repo
	return nil
}

// Poll fetches from remote and returns the current HEAD SHA. Changed is true
// if HEAD differs from previousSHA.
func (p *Poller) Poll(ctx context.Context, previousSHA string) (*PollResult, error) {
	slog.Debug("polling git repository", "url", p.repoURL, "branch", p.branch)
	if err := p.fetch(ctx); err != nil {
		return nil, err
	}

	head, err := p.headSHA()
	if err != nil {
		return nil, err
	}

	slog.Debug("git fetch complete", "sha", head, "changed", head != previousSHA)
	return &PollResult{
		HeadSHA: head,
		Changed: head != previousSHA,
	}, nil
}

func (p *Poller) fetch(ctx context.Context) error {
	auth, err := p.auth.GitAuth(ctx)
	if err != nil {
		return err
	}

	err = p.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("+refs/heads/" + p.branch + ":refs/remotes/origin/" + p.branch)},
		Auth:       auth,
		Force:      true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("fetching: %w", err)
	}

	// Reset working tree to remote branch HEAD
	wt, err := p.repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}

	remoteRef, err := p.repo.Reference(plumbing.NewRemoteReferenceName("origin", p.branch), true)
	if err != nil {
		return fmt.Errorf("resolving remote ref: %w", err)
	}

	err = wt.Reset(&git.ResetOptions{
		Commit: remoteRef.Hash(),
		Mode:   git.HardReset,
	})
	if err != nil {
		return fmt.Errorf("resetting to remote HEAD: %w", err)
	}
	if err := wt.Clean(&git.CleanOptions{Dir: true}); err != nil {
		return fmt.Errorf("cleaning untracked files: %w", err)
	}

	return nil
}

func (p *Poller) headSHA() (string, error) {
	ref, err := p.repo.Reference(plumbing.NewRemoteReferenceName("origin", p.branch), true)
	if err != nil {
		return "", fmt.Errorf("resolving HEAD: %w", err)
	}
	return ref.Hash().String(), nil
}

// NewStaticTokenAuth creates an AuthProvider for a direct token value.
// For SSH repo URLs it falls back to SSH agent auth.
func NewStaticTokenAuth(repoURL, token string) AuthProvider {
	if IsSSHURL(repoURL) {
		return NewSSHAgentAuth(repoURL)
	}
	return &tokenValueAuth{token: token}
}

// verifyRemote checks that the existing clone's origin URL matches the expected repo URL.
func (p *Poller) verifyRemote(repo *git.Repository) bool {
	remote, err := repo.Remote("origin")
	if err != nil || len(remote.Config().URLs) == 0 {
		slog.Warn("existing repo has no origin remote, re-cloning")
		return false
	}
	if remote.Config().URLs[0] != p.repoURL {
		slog.Warn("existing repo has wrong remote, re-cloning",
			"expected", p.repoURL,
			"got", remote.Config().URLs[0])
		return false
	}
	return true
}
