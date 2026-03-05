package gitpoll

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	p := New(bareDir, "master", localDir, "")
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
		{"", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, isSSHURL(tt.url), "isSSHURL(%q)", tt.url)
	}
}

func TestPollerReopenExisting(t *testing.T) {
	t.Parallel()
	bareDir := initBareRepo(t)
	localDir := filepath.Join(t.TempDir(), "clone")
	ctx := context.Background()

	// First init (clone)
	p1 := New(bareDir, "master", localDir, "")
	require.NoError(t, p1.Init(ctx))

	// Second init (open existing)
	p2 := New(bareDir, "master", localDir, "")
	require.NoError(t, p2.Init(ctx))

	result, err := p2.Poll(ctx, "")
	require.NoError(t, err)
	assert.NotEmpty(t, result.HeadSHA)
}
