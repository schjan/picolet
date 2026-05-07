package reconciler

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
)

// Action describes what to do with a file.
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
	ActionNoop   Action = "noop"
)

// Change represents a single file change.
type Change struct {
	DestPath        string
	Category        string
	Action          Action
	NewContent      string // empty for delete
	OldHash         string // from state, empty for create
	NewHash         string // sha256 of NewContent
	ServiceName     string // "foo.service"; "" for non-quadlets/secrets
	ManifestRelPath string // relative path within manifests/ dir; "" for non-manifests
}

// Changeset is the complete set of changes to apply.
type Changeset struct {
	Changes []Change
	Summary map[Action]int
}

// HasChanges returns true if there are any non-noop changes.
func (cs *Changeset) HasChanges() bool {
	return cs.Summary[ActionCreate] > 0 || cs.Summary[ActionUpdate] > 0 || cs.Summary[ActionDelete] > 0
}

// Diff computes the changeset between desired files and current state.
func Diff(
	desired []resolver.ResolvedFile,
	currentState *state.State,
) *Changeset {
	cs := &Changeset{
		Summary: make(map[Action]int),
	}

	seen := make(map[string]bool)
	for _, f := range desired {
		seen[f.DestPath] = true
		cs.addChange(classifyFile(f, currentState))
	}

	// Files in state but not in desired → delete
	for destPath, mf := range currentState.ManagedFiles {
		if !seen[destPath] {
			cs.addChange(Change{
				DestPath:    destPath,
				Category:    mf.Category,
				Action:      ActionDelete,
				OldHash:     mf.Hash,
				ServiceName: currentState.ServiceNames[destPath], // "" for non-quadlets
			})
		}
	}

	return cs
}

// classifyFile determines the action for a single desired file.
func classifyFile(f resolver.ResolvedFile, currentState *state.State) Change {
	newHash := hash(f.Content)
	mf, managed := currentState.ManagedFiles[f.DestPath]

	c := Change{
		DestPath:        f.DestPath,
		Category:        f.Category,
		OldHash:         mf.Hash,
		NewHash:         newHash,
		ServiceName:     f.ServiceName,
		ManifestRelPath: f.ManifestRelPath,
	}

	switch {
	case !managed:
		c.Action = ActionCreate
		c.NewContent = f.Content
	case mf.Hash == newHash:
		c.Action = ActionNoop
	default:
		c.Action = ActionUpdate
		c.NewContent = f.Content
	}

	return c
}

func (cs *Changeset) addChange(c Change) {
	cs.Changes = append(cs.Changes, c)
	cs.Summary[c.Action]++
}

func hash(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("sha256:%x", h)
}

// SecretNameFromPath extracts the secret name from a "secret:name" dest path.
func SecretNameFromPath(destPath string) string {
	if name, ok := strings.CutPrefix(destPath, "secret:"); ok {
		return name
	}
	return destPath
}

var categories = []string{"container", "network", "volume", "kube", "systemd", "manifest", "secret"}

// Categories returns the fixed set of known file categories used for metric labels.
func Categories() []string {
	return categories
}
