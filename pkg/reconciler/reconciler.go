package reconciler

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
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
	DestPath    string
	Category    string
	Action      Action
	NewContent  string // empty for delete
	OldHash     string // from state, empty for create
	NewHash     string // sha256 of NewContent
	ServiceName string // "foo.service"; "" for non-quadlets/secrets
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
	for destPath := range currentState.ManagedFiles {
		if !seen[destPath] {
			cs.addChange(Change{
				DestPath:    destPath,
				Category:    categoryFromPath(destPath),
				Action:      ActionDelete,
				OldHash:     currentState.ManagedFiles[destPath],
				ServiceName: currentState.ServiceNames[destPath], // "" for non-quadlets
			})
		}
	}

	return cs
}

// classifyFile determines the action for a single desired file.
func classifyFile(f resolver.ResolvedFile, currentState *state.State) Change {
	newHash := hash(f.Content)
	oldHash, managed := currentState.ManagedFiles[f.DestPath]

	if !managed {
		return Change{
			DestPath:    f.DestPath,
			Category:    f.Category,
			Action:      ActionCreate,
			NewContent:  f.Content,
			NewHash:     newHash,
			ServiceName: f.ServiceName,
		}
	}

	if oldHash == newHash {
		return Change{
			DestPath:    f.DestPath,
			Category:    f.Category,
			Action:      ActionNoop,
			OldHash:     oldHash,
			NewHash:     newHash,
			ServiceName: f.ServiceName,
		}
	}

	return Change{
		DestPath:    f.DestPath,
		Category:    f.Category,
		Action:      ActionUpdate,
		NewContent:  f.Content,
		OldHash:     oldHash,
		NewHash:     newHash,
		ServiceName: f.ServiceName,
	}
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

// categoryFromPath guesses the category from a managed file's dest path.
func categoryFromPath(destPath string) string {
	if strings.HasPrefix(destPath, "secret:") {
		return "secret"
	}
	switch filepath.Ext(destPath) {
	case ".container":
		return "container"
	case ".network":
		return "network"
	case ".volume":
		return "volume"
	case ".kube":
		return "kube"
	case ".socket", ".service", ".timer":
		return "systemd"
	}
	if strings.Contains(destPath, "/picolet/manifests/") {
		return "manifest"
	}
	return "unknown"
}
