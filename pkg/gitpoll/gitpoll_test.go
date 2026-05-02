package gitpoll

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type anonymousAuth struct{}

func (anonymousAuth) GitAuth(_ context.Context) (transport.AuthMethod, error) {
	return nil, nil //nolint:nilnil
}

// initBareRepo creates a bare git repo with an initial commit on "main" branch.
func initBareRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bare.git")

	// Init a non-bare repo, commit, then clone bare
	workDir := filepath.Join(t.TempDir(), "work")
	repo, err := git.PlainInit(workDir, false)
	require.NoError(t, err)

	// Create a file and commit
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "fleet.yml"), []byte("images: {}"), 0o600))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("fleet.yml")
	require.NoError(t, err)
	_, err = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com"},
	})
	require.NoError(t, err)

	// Clone as bare
	_, err = git.PlainClone(dir, true, &git.CloneOptions{URL: workDir})
	require.NoError(t, err)
	return dir
}

func addCommitToBareRepo(t *testing.T, bareDir string) string {
	t.Helper()
	workDir := filepath.Join(t.TempDir(), "push-work")
	repo, err := git.PlainClone(workDir, false, &git.CloneOptions{URL: bareDir})
	require.NoError(t, err)

	f := filepath.Join(workDir, "new-file.txt")
	require.NoError(t, os.WriteFile(f, []byte("update"), 0o600))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("new-file.txt")
	require.NoError(t, err)
	hash, err := wt.Commit("second commit", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com"},
	})
	require.NoError(t, err)
	require.NoError(t, repo.Push(&git.PushOptions{}))
	return hash.String()
}

func TestPollerInitAndPoll(t *testing.T) {
	t.Parallel()
	bareDir := initBareRepo(t)
	localDir := filepath.Join(t.TempDir(), "clone")
	ctx := context.Background()

	p := New(bareDir, "master", localDir, anonymousAuth{})
	require.NoError(t, p.Init(ctx))

	// First poll with empty previous SHA — should report changed
	result, err := p.Poll(ctx, "")
	require.NoError(t, err)
	assert.True(t, result.Changed, "expected Changed=true on first poll")
	assert.NotEmpty(t, result.HeadSHA)

	// Poll again with same SHA — no change
	result2, err := p.Poll(ctx, result.HeadSHA)
	require.NoError(t, err)
	assert.False(t, result2.Changed, "expected Changed=false when SHA matches")

	// Push a new commit, poll again
	newSHA := addCommitToBareRepo(t, bareDir)
	result3, err := p.Poll(ctx, result.HeadSHA)
	require.NoError(t, err)
	assert.True(t, result3.Changed, "expected Changed=true after new commit")
	assert.Equal(t, newSHA, result3.HeadSHA)
}

func TestIsSSHURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url  string
		want bool
	}{
		{"ssh://git@github.com/org/repo.git", true},
		{"git+ssh://git@github.com/org/repo.git", true},
		{"git@github.com:org/repo.git", true},
		{"https://github.com/org/repo.git", false},
		{"http://github.com/org/repo.git", false},
		{"/local/path/repo", false},
		{"%%%://bad-url", false},
		{"", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, IsSSHURL(tt.url), "isSSHURL(%q)", tt.url)
	}
}

func TestNewDefaultsToAnonymousAuthWhenNilProvider(t *testing.T) {
	t.Parallel()

	p := New("https://github.com/org/repo.git", "main", "/tmp/clone", nil)
	require.NotNil(t, p.auth)

	auth, err := p.auth.GitAuth(context.Background())
	require.NoError(t, err)
	assert.Nil(t, auth)
}

func TestSSHAuthUser(t *testing.T) {
	t.Parallel()
	tests := []struct{ url, want string }{
		{"git@github.com:org/repo.git", "git"},
		{"ssh://deploy@host/repo.git", "deploy"},
		{"ssh://host/repo.git", "git"},         // no user → default
		{"https://github.com/org/repo", "git"}, // non-SSH → default (won't be called)
	}
	for _, tt := range tests {
		ep, err := transport.NewEndpoint(tt.url)
		user := "git"
		if err == nil && ep.User != "" {
			user = ep.User
		}
		assert.Equal(t, tt.want, user, "url=%s", tt.url)
	}
}

func TestNewWithTokenAuthHTTPS(t *testing.T) {
	t.Parallel()
	p := NewWithToken("https://github.com/org/repo.git", "main", "/tmp/clone", "ghp_secret123")
	auth, err := p.auth.GitAuth(context.Background())
	require.NoError(t, err)
	require.NotNil(t, auth)
	basic, ok := auth.(*http.BasicAuth)
	require.True(t, ok, "expected *http.BasicAuth, got %T", auth)
	assert.Equal(t, "x", basic.Username)
	assert.Equal(t, "ghp_secret123", basic.Password)
}

func TestNewWithTokenSSHTakesPrecedence(t *testing.T) {
	t.Parallel()
	p := NewWithToken("git@github.com:org/repo.git", "main", "/tmp/clone", "ghp_secret123")
	auth, err := p.auth.GitAuth(context.Background())
	if err != nil {
		// SSH agent may not be available in test/CI — skip gracefully
		t.Skipf("SSH agent unavailable: %v", err)
	}
	// SSH URL must return SSH auth, not BasicAuth, even when token is set
	_, isBasic := auth.(*http.BasicAuth)
	assert.False(t, isBasic, "SSH URL should not return BasicAuth even when token is set")
}

func TestPollerReopenExisting(t *testing.T) {
	t.Parallel()
	bareDir := initBareRepo(t)
	localDir := filepath.Join(t.TempDir(), "clone")
	ctx := context.Background()

	// First init (clone)
	p1 := New(bareDir, "master", localDir, anonymousAuth{})
	require.NoError(t, p1.Init(ctx))

	// Second init (open existing)
	p2 := New(bareDir, "master", localDir, anonymousAuth{})
	require.NoError(t, p2.Init(ctx))

	result, err := p2.Poll(ctx, "")
	require.NoError(t, err)
	assert.NotEmpty(t, result.HeadSHA)
}

func TestPollerCleansUntrackedFiles(t *testing.T) {
	t.Parallel()
	bareDir := initBareRepo(t)
	localDir := filepath.Join(t.TempDir(), "clone")
	ctx := context.Background()

	p := New(bareDir, "master", localDir, anonymousAuth{})
	require.NoError(t, p.Init(ctx))

	stalePath := filepath.Join(localDir, "stale.tmpl")
	require.NoError(t, os.WriteFile(stalePath, []byte("stale"), 0o600))
	require.FileExists(t, stalePath)

	_, err := p.Poll(ctx, "")
	require.NoError(t, err)
	require.NoFileExists(t, stalePath)
}
