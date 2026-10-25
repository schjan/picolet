package validator

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"
	"go.yaml.in/yaml/v4"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/status"
)

const unresolvedSecretPlaceholder = "<secret>"

// Target is the Host the validated files deploy to.
type Target struct {
	// Rootless must match the Host's Podman mode so that quadlet conversion
	// generates correct systemd unit dependencies (user vs system session).
	Rootless bool
	// HostDataDir is the host-visible data directory the delivered data
	// files live under (resolver.ResolvedHost.HostDataDir). Empty skips the
	// checks that compare references against delivered data files.
	HostDataDir string
}

// Option configures a validation run.
type Option func(*options)

type options struct {
	logger              *slog.Logger
	allowTestRecipients bool
}

// WithLogger sets the logger validation logs through (progress, Quadlet
// warnings). A nil logger is ignored; the default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.logger = l
		}
	}
}

// AllowTestRecipients turns off ValidateAll's warning about published
// TEST-ONLY recipients (picolet's fixture keys) declared by a Host or an
// Operator, for the example Fleet and test Fleets, which declare them on
// purpose.
func AllowTestRecipients() Option {
	return func(o *options) { o.allowTestRecipients = true }
}

func newOptions(opts []Option) options {
	o := options{logger: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// ValidateFiles validates a set of already-resolved files.
func ValidateFiles(files []resolver.ResolvedFile, target Target, opts ...Option) error {
	_, err := AnalyzeFiles(files, target, opts...)
	return err
}

// AnalyzeFiles validates resolved files and returns the generated systemd
// dependency map keyed by unit name. If validation fails, the returned map
// is partial and the error describes all validation failures.
func AnalyzeFiles(files []resolver.ResolvedFile, target Target, opts ...Option) (map[string]status.UnitDependencies, error) {
	log := newOptions(opts).logger
	unitsInfo := buildUnitsInfoFromFiles(files, target.Rootless, log)
	delivered := newDelivery(files, target.HostDataDir)
	deps := make(map[string]status.UnitDependencies)
	var errs []error
	for _, f := range files {
		d, err := analyzeFile(f, unitsInfo, target.Rootless, log)
		if !d.IsEmpty() {
			if unit := unitNameForAnalysis(f); unit != "" {
				deps[unit] = d
			}
		}
		// Podman accepted the unit, so its references are well-formed.
		if err == nil && f.Category == config.CategoryBuild {
			err = delivered.checkBuild(f.ParsedUnit, f.DestPath)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return deps, errors.Join(errs...)
}

// ValidateHost resolves and validates files for a single host. The resolver
// logs through its own resolver.Config.Logger.
func ValidateHost(ctx context.Context, r *resolver.Resolver, hostname string, opts ...Option) error {
	_, err := validateHost(ctx, r, hostname, opts)
	return err
}

// validateHost is ValidateHost returning the Host's resolve too; it is nil
// exactly when the resolve failed.
func validateHost(ctx context.Context, r *resolver.Resolver, hostname string, opts []Option) (*resolver.ResolvedHost, error) {
	log := newOptions(opts).logger
	log.Info("validating host", "host", hostname)
	resolved, err := r.ResolveHost(ctx, hostname)
	if err != nil {
		return nil, fmt.Errorf("host %s: resolve: %w", hostname, err)
	}
	if err := ValidateFiles(resolved.Files, Target{Rootless: r.Rootless(), HostDataDir: resolved.HostDataDir}, opts...); err != nil {
		return resolved, fmt.Errorf("host %s: %w", hostname, err)
	}
	log.Info("host validated", "host", hostname, "files", len(resolved.Files))
	return resolved, nil
}

// ValidateAll resolves and validates all hosts in the fleet config, then runs
// the Fleet-wide passes over fsys, the Fleet repository r and cfg were
// loaded from. Intended for CI/CD pipelines; agents use ValidateFiles
// directly.
func ValidateAll(ctx context.Context, fsys fs.FS, r *resolver.Resolver, cfg *config.Config, opts ...Option) error {
	o := newOptions(opts)
	var errs []error
	var resolved []*resolver.ResolvedHost
	var unresolved []string
	for _, hostname := range cfg.SortedHostnames() {
		rh, err := validateHost(ctx, r, hostname, opts)
		if err != nil {
			errs = append(errs, err)
		}
		if rh == nil {
			unresolved = append(unresolved, hostname)
			continue
		}
		resolved = append(resolved, rh)
	}
	// Fleet-wide passes, after every Host has been resolved.
	if err := validateAgentBundles(ctx, fsys, cfg, o.logger); err != nil {
		errs = append(errs, err)
	}
	aud, err := loadAudience(fsys, cfg, resolved, unresolved, o)
	if err != nil {
		errs = append(errs, err)
	}
	if err := validateSOPSFiles(fsys, aud); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func analyzeFile(f resolver.ResolvedFile, unitsInfo map[string]*quadlet.UnitInfo, rootless bool, log *slog.Logger) (status.UnitDependencies, error) {
	log.Debug("validating file", "path", f.DestPath, "category", f.Category)
	spec, ok := config.SpecFor(f.Category)
	if !ok {
		return status.UnitDependencies{}, fmt.Errorf("%s: unknown file category %q", f.DestPath, f.Category)
	}
	switch spec.Check {
	case config.CheckQuadlet:
		return analyzeQuadlet(f, unitsInfo, rootless, log)
	case config.CheckManifest:
		return status.UnitDependencies{}, validateManifest(f.DestPath, []byte(f.Content))
	case config.CheckSystemd:
		if err := validateSystemdUnit(f.DestPath, f.Content); err != nil {
			return status.UnitDependencies{}, err
		}
		return dependenciesFromSystemd(f), nil
	case config.CheckSecret:
		return status.UnitDependencies{}, validateSecret(f)
	case config.CheckFile:
		return status.UnitDependencies{}, validateFile(f)
	default:
		return status.UnitDependencies{}, unsupportedError(f.DestPath, filepath.Ext(f.DestPath))
	}
}

func analyzeQuadlet(f resolver.ResolvedFile, unitsInfo map[string]*quadlet.UnitInfo, rootless bool, log *slog.Logger) (status.UnitDependencies, error) {
	if f.ParsedUnit == nil {
		return status.UnitDependencies{}, fmt.Errorf("%s: quadlet unit could not be parsed (invalid INI syntax)", f.DestPath)
	}
	generated, err := convertQuadlet(f.ParsedUnit, unitsInfo, rootless, log)
	if err != nil {
		return status.UnitDependencies{}, err
	}
	if err := rejectSelfPodMember(f.ParsedUnit); err != nil {
		return status.UnitDependencies{}, err
	}
	return dependenciesFromUnit(generated), nil
}

// buildUnitsInfoFromFiles builds a UnitInfo map from resolved files for cross-reference resolution.
// It mirrors Podman's two-phase generateUnitsInfoMap + Convert* flow:
//  1. Pre-populate all unit entries (with ServiceName; ResourceName for Prefill categories).
//  2. Convert the units of PreConvert categories, in ConvertOrder, to
//     populate their ResourceName so that units referencing them (e.g. a
//     container's Network=/Volume=) resolve the cross-reference, and to
//     record pod membership: ConvertContainer appends each Pod= member to the
//     pod's ContainersToStart, which ConvertPod (converted last) turns into
//     Wants=/Before=. Per-file validation converts containers again, appending
//     the members a second time; dependenciesFromUnit deduplicates them.
func buildUnitsInfoFromFiles(files []resolver.ResolvedFile, rootless bool, log *slog.Logger) map[string]*quadlet.UnitInfo {
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

	// Pass 2: Convert* sets ResourceName as a side effect, which later
	// conversions read from the unitsInfoMap.
	// Errors here are not fatal: the same unit will be validated individually in
	// ValidateFiles and will produce a proper error there. We log so the root cause
	// is visible even if the container validation reports it with a less precise message.
	for _, f := range preConvertFiles(files) {
		_, _, err := quadletConverters[f.Category].convert(f.ParsedUnit, units, rootless)
		if err != nil {
			log.Debug("pre-populating units info: conversion failed, will be reported in per-file validation",
				"file", f.DestPath, "error", err)
		}
	}

	return units
}

// preConvertFiles selects the parsed units whose category is converted by
// Podman and marked PreConvert, stably ordered by the category's ConvertOrder.
func preConvertFiles(files []resolver.ResolvedFile) []resolver.ResolvedFile {
	var selected []resolver.ResolvedFile
	for _, f := range files {
		spec, _ := config.SpecFor(f.Category)
		if f.ParsedUnit != nil && spec.Check == config.CheckQuadlet && spec.PreConvert {
			selected = append(selected, f)
		}
	}
	slices.SortStableFunc(selected, func(a, b resolver.ResolvedFile) int {
		sa, _ := config.SpecFor(a.Category)
		sb, _ := config.SpecFor(b.Category)
		return cmp.Compare(sa.ConvertOrder, sb.ConvertOrder)
	})
	return selected
}

func validateSystemdUnit(path, content string) error {
	if len(strings.TrimSpace(content)) == 0 {
		return fmt.Errorf("%s: empty systemd unit", path)
	}
	// Basic structural check: must contain at least one section header
	if !strings.Contains(content, "[") {
		return fmt.Errorf("%s: no section headers found in systemd unit", path)
	}
	// Light timer check: a [Timer] section must declare at least one On*= trigger,
	// otherwise systemd refuses to start the timer. Parse via the INI parser so the
	// check is robust against comments and value-position "[Timer]" strings.
	unit := parser.NewUnitFile()
	unit.Filename = filepath.Base(path)
	if err := unit.Parse(content); err != nil {
		return fmt.Errorf("%s: invalid systemd unit syntax: %w", path, err)
	}
	if unit.HasGroup("Timer") && !timerHasTrigger(unit) {
		return fmt.Errorf("%s: [Timer] section requires an On*= trigger (e.g. OnCalendar=, OnBootSec=, OnUnitActiveSec=)", path)
	}
	return nil
}

// timerTriggerKeys are the systemd [Timer] keys that schedule an elapse. A timer
// must declare at least one or systemd refuses to start it. See systemd.timer(5).
var timerTriggerKeys = map[string]struct{}{
	"OnActiveSec":       {},
	"OnBootSec":         {},
	"OnStartupSec":      {},
	"OnUnitActiveSec":   {},
	"OnUnitInactiveSec": {},
	"OnCalendar":        {},
	"OnClockChange":     {},
	"OnTimezoneChange":  {},
}

// timerHasTrigger reports whether a [Timer] section declares at least one known
// trigger key. Matching against the known set (rather than an "On" prefix) rejects
// typos like OnCalender= that systemd would silently fail to schedule.
func timerHasTrigger(unit *parser.UnitFile) bool {
	for _, key := range unit.ListKeys("Timer") {
		if _, ok := timerTriggerKeys[key]; ok {
			return true
		}
	}
	return false
}

func dependenciesFromSystemd(f resolver.ResolvedFile) status.UnitDependencies {
	unit := parser.NewUnitFile()
	unit.Filename = filepath.Base(f.DestPath)
	if err := unit.Parse(f.Content); err != nil {
		return status.UnitDependencies{}
	}
	return dependenciesFromUnit(unit)
}

func dependenciesFromUnit(unit *parser.UnitFile) status.UnitDependencies {
	return status.UnitDependencies{
		Requires: lookupDependency(unit, "Requires"),
		Wants:    lookupDependency(unit, "Wants"),
		After:    lookupDependency(unit, "After"),
		Before:   lookupDependency(unit, "Before"),
		BindsTo:  lookupDependency(unit, "BindsTo"),
		PartOf:   lookupDependency(unit, "PartOf"),
	}
}

func lookupDependency(unit *parser.UnitFile, key string) []string {
	values := unit.LookupAllStrv("Unit", key)
	if len(values) == 0 {
		return nil
	}
	slices.Sort(values)
	return slices.Compact(values)
}

func unitNameForAnalysis(f resolver.ResolvedFile) string {
	if f.ServiceName != "" {
		return f.ServiceName
	}
	if f.ParsedUnit != nil {
		if info := buildUnitInfo(f.ParsedUnit); info != nil {
			return info.ServiceFileName()
		}
	}
	if spec, _ := config.SpecFor(f.Category); spec.Unit == config.FileUnit {
		return filepath.Base(f.DestPath)
	}
	return ""
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
	return isYAMLSource(f.SrcPath)
}

// validateFile checks opaque container-mounted files. Empty content is allowed
// (legitimate for empty allowlists/rule sets). Files whose source extension is
// .yml or .yaml (after stripping .tmpl) are syntax-checked; anything else is
// considered opaque and not inspected.
func validateFile(f resolver.ResolvedFile) error {
	if !isYAMLSource(f.SrcPath) {
		return nil
	}
	return validateYAMLSyntax(f.DestPath, []byte(f.Content))
}

// isYAMLSource reports whether a source path's effective extension (after
// stripping a trailing .tmpl) is .yml or .yaml.
func isYAMLSource(srcPath string) bool {
	effective := config.TrimTemplateSuffix(srcPath)
	switch strings.ToLower(filepath.Ext(effective)) {
	case ".yml", ".yaml":
		return true
	}
	return false
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
