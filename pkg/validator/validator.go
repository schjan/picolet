package validator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/quadlet"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
)

// Validator validates resolved files.
type Validator struct{}

// New creates a new Validator.
func New() *Validator {
	return &Validator{}
}

// ValidateHost resolves and validates files for a single host.
func (v *Validator) ValidateHost(_ context.Context, r *resolver.Resolver, hostname string) error {
	slog.Info("validating host", "host", hostname)
	resolved, err := r.ResolveHost(hostname)
	if err != nil {
		return fmt.Errorf("host %s: resolve: %w", hostname, err)
	}

	unitsInfo := buildUnitsInfoFromFiles(resolved.Files)

	var errs []error
	for _, f := range resolved.Files {
		if err := v.validateFile(f, unitsInfo); err != nil {
			errs = append(errs, fmt.Errorf("host %s: %s: %w", hostname, f.SrcPath, err))
		}
	}
	slog.Info("host validated", "host", hostname, "files", len(resolved.Files))
	return errors.Join(errs...)
}

// ValidateAll resolves and validates all hosts in the fleet config.
// Intended for CI/CD pipelines; agents should use ValidateHost instead.
func (v *Validator) ValidateAll(ctx context.Context, r *resolver.Resolver, cfg *config.Config) error {
	var errs []error
	for _, hostname := range cfg.SortedHostnames() {
		if err := v.ValidateHost(ctx, r, hostname); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (v *Validator) validateFile(f resolver.ResolvedFile, unitsInfo map[string]*quadlet.UnitInfo) error {
	switch f.Category {
	case "network", "volume", "container", "kube":
		return v.validateQuadlet(f.DestPath, []byte(f.Content), unitsInfo)
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

// buildUnitsInfoFromFiles builds a UnitInfo map from resolved files for cross-reference resolution.
func buildUnitsInfoFromFiles(files []resolver.ResolvedFile) map[string]*quadlet.UnitInfo {
	units := make(map[string]*quadlet.UnitInfo)
	for _, f := range files {
		ext := filepath.Ext(f.DestPath)
		filename := filepath.Base(f.DestPath)
		if info := BuildUnitInfo(filename, ext); info != nil {
			units[filename] = info
		}
	}
	return units
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
