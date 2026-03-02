package validator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/containers/podman/v5/pkg/systemd/quadlet"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
)

// Validator validates resolved files.
type Validator struct {
	mu           sync.Mutex
	unitsInfo    map[string]*quadlet.UnitInfo
	currentFiles []resolver.ResolvedFile
}

// New creates a new Validator.
func New() *Validator {
	return &Validator{}
}

// ValidateAll resolves and validates all hosts.
func (v *Validator) ValidateAll(_ context.Context, r *resolver.Resolver, cfg *config.Config) error {
	var errs []error

	for _, hostname := range cfg.SortedHostnames() {
		slog.Info("validating host", "host", hostname)
		resolved, err := r.ResolveHost(hostname)
		if err != nil {
			errs = append(errs, fmt.Errorf("host %s: resolve: %w", hostname, err))
			continue
		}

		// Set current files for cross-reference resolution (unitsInfoMap)
		v.currentFiles = resolved.Files
		v.unitsInfo = nil

		for _, f := range resolved.Files {
			if err := v.validateFile(f); err != nil {
				errs = append(errs, fmt.Errorf("host %s: %s: %w", hostname, f.SrcPath, err))
			}
		}
		slog.Info("host validated", "host", hostname, "files", len(resolved.Files))
	}

	return errors.Join(errs...)
}

func (v *Validator) validateFile(f resolver.ResolvedFile) error {
	switch f.Category {
	case "network", "volume", "container", "kube":
		return v.validateQuadlet(f.DestPath, []byte(f.Content))
	case "manifest":
		return v.validateManifest(f.DestPath, []byte(f.Content))
	case "systemd":
		return v.validateSystemdUnit(f.DestPath, f.Content)
	case "secret":
		return v.validateSecret(f.DestPath, f.Content)
	default:
		slog.Warn("unknown file category, skipping validation", "category", f.Category, "path", f.SrcPath)
		return nil
	}
}

func (v *Validator) validateSystemdUnit(path, content string) error {
	if len(strings.TrimSpace(content)) == 0 {
		return fmt.Errorf("%s: empty systemd unit", path)
	}
	// Basic structural check: must contain at least one section header
	if !strings.Contains(content, "[") {
		return fmt.Errorf("%s: no section headers found in systemd unit", path)
	}
	return nil
}

func (v *Validator) validateSecret(path, content string) error {
	if len(strings.TrimSpace(content)) == 0 {
		return fmt.Errorf("%s: empty secret content", path)
	}
	return nil
}
