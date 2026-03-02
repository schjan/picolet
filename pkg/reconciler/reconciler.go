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
	DestPath   string
	Category   string
	Action     Action
	NewContent string // empty for delete
	OldHash    string // from state, empty for create
	NewHash    string // sha256 of NewContent
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

// Reconciler computes the diff between desired and current state.
type Reconciler struct{}

// New creates a new Reconciler.
func New() *Reconciler {
	return &Reconciler{}
}

// SecretChecker checks if a Podman secret already exists.
type SecretChecker func(name string) (bool, error)

// Diff computes the changeset between desired files and current state.
// secretChecker is used for skip_if_exists secrets; pass nil if not needed.
func (r *Reconciler) Diff(
	desired []resolver.ResolvedFile,
	currentState *state.State,
	secretChecker SecretChecker,
) *Changeset {
	cs := &Changeset{
		Summary: make(map[Action]int),
	}

	seen := make(map[string]bool)
	for _, f := range desired {
		seen[f.DestPath] = true
		cs.addChange(classifyFile(f, currentState, secretChecker))
	}

	// Files in state but not in desired → delete
	for destPath := range currentState.ManagedFiles {
		if !seen[destPath] {
			cs.addChange(Change{
				DestPath: destPath,
				Category: categoryFromPath(destPath),
				Action:   ActionDelete,
				OldHash:  currentState.ManagedFiles[destPath],
			})
		}
	}

	return cs
}

// classifyFile determines the action for a single desired file.
func classifyFile(f resolver.ResolvedFile, currentState *state.State, secretChecker SecretChecker) Change {
	newHash := hash(f.Content)
	oldHash, managed := currentState.ManagedFiles[f.DestPath]

	// For secrets with skip_if_exists: if already managed AND exists in Podman → noop
	if f.Category == "secret" && managed && secretChecker != nil {
		secretName := SecretNameFromPath(f.DestPath)
		exists, err := secretChecker(secretName)
		if err == nil && exists {
			return Change{
				DestPath: f.DestPath, Category: f.Category,
				Action: ActionNoop, OldHash: oldHash, NewHash: newHash,
			}
		}
	}

	if !managed {
		return Change{
			DestPath: f.DestPath, Category: f.Category,
			Action: ActionCreate, NewContent: f.Content, NewHash: newHash,
		}
	}

	if oldHash == newHash {
		return Change{
			DestPath: f.DestPath, Category: f.Category,
			Action: ActionNoop, OldHash: oldHash, NewHash: newHash,
		}
	}

	return Change{
		DestPath: f.DestPath, Category: f.Category,
		Action: ActionUpdate, NewContent: f.Content, OldHash: oldHash, NewHash: newHash,
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
	if strings.HasPrefix(destPath, "/var/lib/picolet/manifests/") {
		return "manifest"
	}
	return "unknown"
}
