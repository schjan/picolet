package validator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/quadlet"
	"go.yaml.in/yaml/v4"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
)

const unresolvedSecretPlaceholder = "<secret>"

// ValidateFiles validates a set of already-resolved files.
// rootless must match the target host's Podman mode so that quadlet conversion
// generates correct systemd unit dependencies (user vs system session).
func ValidateFiles(files []resolver.ResolvedFile, rootless bool) error {
	unitsInfo := buildUnitsInfoFromFiles(files, rootless)
	var errs []error
	for _, f := range files {
		if err := validateFile(f, unitsInfo, rootless); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ValidateHost resolves and validates files for a single host.
func ValidateHost(ctx context.Context, r *resolver.Resolver, hostname string) error {
	slog.Info("validating host", "host", hostname)
	resolved, err := r.ResolveHost(ctx, hostname)
	if err != nil {
		return fmt.Errorf("host %s: resolve: %w", hostname, err)
	}
	if err := ValidateFiles(resolved.Files, r.Rootless()); err != nil {
		return fmt.Errorf("host %s: %w", hostname, err)
	}
	slog.Info("host validated", "host", hostname, "files", len(resolved.Files))
	return nil
}

// ValidateAll resolves and validates all hosts in the fleet config.
// Intended for CI/CD pipelines; agents use ValidateFiles directly.
func ValidateAll(ctx context.Context, r *resolver.Resolver, cfg *config.Config) error {
	var errs []error
	for _, hostname := range cfg.SortedHostnames() {
		if err := ValidateHost(ctx, r, hostname); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validateFile(f resolver.ResolvedFile, unitsInfo map[string]*quadlet.UnitInfo, rootless bool) error {
	slog.Debug("validating file", "path", f.DestPath, "category", f.Category)
	switch f.Category {
	case "network", "volume", "container", "kube":
		if f.ParsedUnit == nil {
			return fmt.Errorf("%s: quadlet unit could not be parsed (invalid INI syntax)", f.DestPath)
		}
		return validateQuadlet(f.ParsedUnit, unitsInfo, rootless)
	case "manifest":
		return validateManifest(f.DestPath, []byte(f.Content))
	case "systemd":
		return validateSystemdUnit(f.DestPath, f.Content)
	case "secret":
		return validateSecret(f)
	default:
		return fmt.Errorf("%s: unknown file category %q", f.DestPath, f.Category)
	}
}

// buildUnitsInfoFromFiles builds a UnitInfo map from resolved files for cross-reference resolution.
// It mirrors Podman's two-phase generateUnitsInfoMap + Convert* flow:
//  1. Pre-populate all unit entries (with ServiceName; ResourceName for containers).
//  2. Call Convert* for network/volume/kube to populate their ResourceName so that
//     containers referencing them can resolve the cross-reference via ConvertContainer.
func buildUnitsInfoFromFiles(files []resolver.ResolvedFile, rootless bool) map[string]*quadlet.UnitInfo {
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
			_, _, err = quadlet.ConvertNetwork(f.ParsedUnit, units, rootless)
		case "volume":
			_, _, err = quadlet.ConvertVolume(f.ParsedUnit, units, rootless)
		case "kube":
			_, err = quadlet.ConvertKube(f.ParsedUnit, units, rootless)
		}
		if err != nil {
			slog.Debug("pre-populating units info: conversion failed, will be reported in per-file validation",
				"file", f.DestPath, "error", err)
		}
	}

	return units
}

func validateSystemdUnit(path, content string) error {
	if len(strings.TrimSpace(content)) == 0 {
		return fmt.Errorf("%s: empty systemd unit", path)
	}
	// Basic structural check: must contain at least one section header
	if !strings.Contains(content, "[") {
		return fmt.Errorf("%s: no section headers found in systemd unit", path)
	}
	return nil
}

func validateSecret(f resolver.ResolvedFile) error {
	content := strings.TrimSpace(f.Content)
	if content == "" {
		return fmt.Errorf("%s: empty secret content", f.DestPath)
	}
	if !shouldValidateSecretYAML(f, content) {
		return nil
	}
	if err := validateYAMLSyntax(f.DestPath, []byte(f.Content)); err != nil {
		return err
	}
	return nil
}

func shouldValidateSecretYAML(f resolver.ResolvedFile, trimmedContent string) bool {
	if trimmedContent == unresolvedSecretPlaceholder {
		return false
	}
	effectivePath := strings.TrimSuffix(strings.ToLower(f.SrcPath), ".tmpl")
	switch filepath.Ext(effectivePath) {
	case ".yml", ".yaml":
		return true
	default:
		return false
	}
}

func validateYAMLSyntax(path string, content []byte) error {
	docs, err := splitYAMLDocuments(content)
	if err != nil {
		return fmt.Errorf("%s: YAML parse error: %w", path, err)
	}
	for docIdx, doc := range docs {
		var decoded any
		if err := yaml.Unmarshal(doc, &decoded); err != nil {
			return fmt.Errorf("%s: document %d: YAML parse error: %w", path, docIdx+1, err)
		}
	}
	return nil
}
