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

// ValidateFiles validates a set of already-resolved files.
// Callers with already-resolved files should use this directly to avoid double resolution.
func (v *Validator) ValidateFiles(files []resolver.ResolvedFile) error {
	unitsInfo := buildUnitsInfoFromFiles(files)
	var errs []error
	for _, f := range files {
		if err := v.validateFile(f, unitsInfo); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ValidateHost resolves and validates files for a single host.
func (v *Validator) ValidateHost(_ context.Context, r *resolver.Resolver, hostname string) error {
	slog.Info("validating host", "host", hostname)
	resolved, err := r.ResolveHost(hostname)
	if err != nil {
		return fmt.Errorf("host %s: resolve: %w", hostname, err)
	}
	if err := v.ValidateFiles(resolved.Files); err != nil {
		return fmt.Errorf("host %s: %w", hostname, err)
	}
	slog.Info("host validated", "host", hostname, "files", len(resolved.Files))
	return nil
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
		if f.ParsedUnit == nil {
			return fmt.Errorf("%s: parse failed (content invalid)", f.DestPath)
		}
		return v.validateQuadlet(f.ParsedUnit, unitsInfo)
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
// It mirrors Podman's two-phase generateUnitsInfoMap + Convert* flow:
//  1. Pre-populate all unit entries (with ServiceName; ResourceName for containers).
//  2. Call Convert* for network/volume/kube to populate their ResourceName so that
//     containers referencing them can resolve the cross-reference via ConvertContainer.
func buildUnitsInfoFromFiles(files []resolver.ResolvedFile) map[string]*quadlet.UnitInfo {
	units := make(map[string]*quadlet.UnitInfo)

	// Pass 1: build initial info entries
	for _, f := range files {
		if f.ParsedUnit == nil {
			continue
		}
		filename := filepath.Base(f.DestPath)
		if info := buildUnitInfo(f.ParsedUnit); info != nil {
			units[filename] = info
		}
	}

	// Pass 2: run Convert* for non-container quadlet types to populate ResourceName.
	// This is required because ConvertContainer reads ResourceName from the unitsInfoMap
	// when resolving Network=/Volume= references, and Convert{Network,Volume,Kube}
	// sets it as a side effect.
	// Errors here are not fatal: the same unit will be validated individually in
	// ValidateFiles and will produce a proper error there. We log so the root cause
	// is visible even if the container validation reports it with a less precise message.
	for _, f := range files {
		if f.ParsedUnit == nil {
			continue
		}
		var err error
		switch f.Category {
		case "network":
			_, _, err = quadlet.ConvertNetwork(f.ParsedUnit, units, false)
		case "volume":
			_, _, err = quadlet.ConvertVolume(f.ParsedUnit, units, false)
		case "kube":
			_, err = quadlet.ConvertKube(f.ParsedUnit, units, false)
		}
		if err != nil {
			slog.Debug("pre-populating units info: conversion failed, will be reported in per-file validation",
				"file", f.DestPath, "error", err)
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
