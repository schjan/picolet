package gitpoll

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Poller manages a local clone of a remote git repo and polls for changes.
type Poller struct {
	repoURL   string
	branch    string
	localPath string
	tokenPath string
	repo      *git.Repository
}

// PollResult indicates the current HEAD SHA and whether it changed.
type PollResult struct {
	HeadSHA string
	Changed bool
}

// New creates a poller. localPath is where the repo is cloned to.
func New(repoURL, branch, localPath, tokenPath string) *Poller {
	return &Poller{
		repoURL:   repoURL,
		branch:    branch,
		localPath: localPath,
		tokenPath: tokenPath,
	}
}

// Init opens an existing clone or clones fresh.
func (p *Poller) Init(ctx context.Context) error {
	repo, err := git.PlainOpen(p.localPath)
	if err == nil {
		p.repo = repo
		return p.fetch(ctx)
	}

	auth, err := p.auth()
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
	if err := p.fetch(ctx); err != nil {
		return nil, err
	}

	head, err := p.headSHA()
	if err != nil {
		return nil, err
	}

	return &PollResult{
		HeadSHA: head,
		Changed: head != previousSHA,
	}, nil
}

func (p *Poller) fetch(ctx context.Context) error {
	auth, err := p.auth()
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

	return nil
}

func (p *Poller) headSHA() (string, error) {
	ref, err := p.repo.Reference(plumbing.NewRemoteReferenceName("origin", p.branch), true)
	if err != nil {
		return "", fmt.Errorf("resolving HEAD: %w", err)
	}
	return ref.Hash().String(), nil
}

func (p *Poller) auth() (*http.BasicAuth, error) {
	if p.tokenPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(p.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("reading token from %s: %w", p.tokenPath, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, nil
	}
	return &http.BasicAuth{
		Username: "x",
		Password: token,
	}, nil
}
