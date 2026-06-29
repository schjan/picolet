package reconciler

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"github.com/schjan/picolet/pkg/config"
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
	Category    config.Category
	Action      Action
	NewContent  string // empty for delete
	OldHash     string // from state, empty for create
	NewHash     string // sha256 of NewContent
	ServiceName string // "foo.service"; "" for non-quadlets/secrets
	RelPath     string // relative to category dir; "" for non-manifest/file changes
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
				ServiceName: currentState.ServiceNames[destPath], // set for quadlets and raw systemd units; "" otherwise
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
		DestPath:    f.DestPath,
		Category:    f.Category,
		OldHash:     mf.Hash,
		NewHash:     newHash,
		ServiceName: f.ServiceName,
		RelPath:     f.RelPath,
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

// MergeChangeset overlays applied changes onto an existing state without
// removing entries for paths the changeset did not touch. Unlike agent.UpdateState,
// this is suitable for partial applies such as bootstrap's picolet-only seed.
func MergeChangeset(st *state.State, changeset *Changeset) {
	if st.ManagedFiles == nil {
		st.ManagedFiles = make(map[string]state.ManagedFile)
	}
	if st.ServiceNames == nil {
		st.ServiceNames = make(map[string]string)
	}
	for _, change := range changeset.Changes {
		switch change.Action {
		case ActionDelete:
			delete(st.ManagedFiles, change.DestPath)
			delete(st.ServiceNames, change.DestPath)
		case ActionCreate, ActionUpdate:
			st.ManagedFiles[change.DestPath] = state.ManagedFile{Hash: change.NewHash, Category: change.Category}
			if change.ServiceName != "" {
				st.ServiceNames[change.DestPath] = change.ServiceName
			} else {
				delete(st.ServiceNames, change.DestPath)
			}
		}
	}
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

var categories = []config.Category{
	config.CategoryContainer,
	config.CategoryNetwork,
	config.CategoryVolume,
	config.CategoryKube,
	config.CategorySystemd,
	config.CategoryManifest,
	config.CategoryFile,
	config.CategorySecret,
}

// Categories returns the fixed set of known file categories used for metric labels.
func Categories() []config.Category {
	return slices.Clone(categories)
}
