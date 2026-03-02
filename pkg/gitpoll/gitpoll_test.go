package gitpoll

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// initBareRepo creates a bare git repo with an initial commit on "main" branch.
func initBareRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bare.git")

	// Init a non-bare repo, commit, then clone bare
	workDir := filepath.Join(t.TempDir(), "work")
	repo, err := git.PlainInit(workDir, false)
	if err != nil {
		t.Fatal(err)
	}

	// Create a file and commit
	if err := os.WriteFile(filepath.Join(workDir, "fleet.yml"), []byte("images: {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("fleet.yml"); err != nil {
		t.Fatal(err)
	}
	_, err = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Clone as bare
	_, err = git.PlainClone(dir, true, &git.CloneOptions{URL: workDir})
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

func addCommitToBareRepo(t *testing.T, bareDir string) string {
	t.Helper()
	// Clone, modify, push back
	workDir := filepath.Join(t.TempDir(), "push-work")
	repo, err := git.PlainClone(workDir, false, &git.CloneOptions{URL: bareDir})
	if err != nil {
		t.Fatal(err)
	}

	f := filepath.Join(workDir, "new-file.txt")
	if err := os.WriteFile(f, []byte("update"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("new-file.txt"); err != nil {
		t.Fatal(err)
	}
	hash, err := wt.Commit("second commit", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	return hash.String()
}

func TestPollerInitAndPoll(t *testing.T) {
	bareDir := initBareRepo(t)
	localDir := filepath.Join(t.TempDir(), "clone")
	ctx := context.Background()

	p := New(bareDir, "master", localDir, "")
	if err := p.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// First poll with empty previous SHA — should report changed
	result, err := p.Poll(ctx, "")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !result.Changed {
		t.Error("expected Changed=true on first poll")
	}
	if result.HeadSHA == "" {
		t.Error("HeadSHA should not be empty")
	}

	// Poll again with same SHA — no change
	result2, err := p.Poll(ctx, result.HeadSHA)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if result2.Changed {
		t.Error("expected Changed=false when SHA matches")
	}

	// Push a new commit, poll again
	newSHA := addCommitToBareRepo(t, bareDir)
	result3, err := p.Poll(ctx, result.HeadSHA)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !result3.Changed {
		t.Error("expected Changed=true after new commit")
	}
	if result3.HeadSHA != newSHA {
		t.Errorf("HeadSHA = %q, want %q", result3.HeadSHA, newSHA)
	}
}

func TestPollerReopenExisting(t *testing.T) {
	bareDir := initBareRepo(t)
	localDir := filepath.Join(t.TempDir(), "clone")
	ctx := context.Background()

	// First init (clone)
	p1 := New(bareDir, "master", localDir, "")
	if err := p1.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Second init (open existing)
	p2 := New(bareDir, "master", localDir, "")
	if err := p2.Init(ctx); err != nil {
		t.Fatalf("Init reopen: %v", err)
	}

	result, err := p2.Poll(ctx, "")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if result.HeadSHA == "" {
		t.Error("HeadSHA should not be empty")
	}
}
