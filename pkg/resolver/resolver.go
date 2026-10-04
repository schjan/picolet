package resolver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"
	"go.yaml.in/yaml/v4"

	"github.com/schjan/picolet/pkg/config"
	op "github.com/schjan/picolet/pkg/onepassword"
	pp "github.com/schjan/picolet/pkg/protonpass"
)

// ResolvedFile represents a single rendered file with its destination path.
type ResolvedFile struct {
	// SrcPath is the source template/file path within the repo.
	SrcPath string
	// DestPath is where the file should be deployed on the target host.
	DestPath string
	// Content is the rendered file content.
	Content string
	// Category describes the file type (network, container, kube, manifest, etc.).
	Category config.Category
	// ParsedUnit is the parsed quadlet unit file; nil for non-quadlet files or on parse error.
	ParsedUnit *parser.UnitFile
	// ServiceName is the derived systemd service name (e.g. "foo.service"); "" for non-quadlets.
	ServiceName string
	// RelPath is the file's path relative to its bundle category directory
	// (e.g. "config/scrape.yml" for a manifest at manifests/config/scrape.yml).
	// Only set for manifest- and file-category resolved files.
	RelPath string
}

// ResolvedHost is the complete desired state for a single host.
type ResolvedHost struct {
	Hostname string
	Host     *config.HostConfig
	Files    []ResolvedFile
	Hooks    []config.Hook
	// DataDir is the data directory picolet writes files and manifests to
	// (their DestPath). HostDataDir is the host-visible path of it that the
	// filePath and manifestPath helpers emit (Config.HostDataDir, else DataDir).
	DataDir     string
	HostDataDir string
}

// Config holds configuration for creating a Resolver.
type Config struct {
	FS             fs.FS
	Config         *config.Config
	SecretReader   SecretReader
	OpSecretReader SecretRefReader
	PPSecretReader SecretRefReader
	Rootless       bool
	Strict         bool

	// QuadletDir, SystemdDir, and DataDir override the defaults computed by
	// ResolveDirs. Empty fields fall back to the default for the given
	// Rootless mode. Used by tests to isolate destination paths from a
	// shared host filesystem; production callers leave them empty.
	QuadletDir string
	SystemdDir string
	DataDir    string

	// HostDataDir is the path the filePath/manifestPath template helpers
	// emit. It does NOT change where picolet writes files (that stays DataDir).
	// Empty falls back to DataDir, so native deployments are unaffected; set it
	// when picolet runs containerized and the host sees the data dir at a
	// different path than picolet does.
	HostDataDir string
}

// Resolver renders templates and resolves the desired state for hosts.
type Resolver struct {
	fsys           fs.FS
	cfg            *config.Config
	secretReader   SecretReader
	opSecretReader SecretRefReader
	ppSecretReader SecretRefReader
	quadletDir     string
	systemdDir     string
	dataDir        string
	hostDataDir    string
	rootless       bool
	strict         bool
}

// Rootless reports whether the resolver is configured for rootless mode.
func (r *Resolver) Rootless() bool { return r.rootless }

// New creates a new Resolver.
// Pass nil for SecretReader to use placeholder mode (validate/CI).
// When Rootless is true, destination paths use ~/.config/ and ~/.local/share/ instead of /etc/ and /var/lib/.
func New(rc Config) (*Resolver, error) {
	quadletDir, systemdDir, dataDir, err := ResolveDirs(rc.Rootless)
	if err != nil {
		return nil, err
	}
	if rc.QuadletDir != "" {
		quadletDir = rc.QuadletDir
	}
	if rc.SystemdDir != "" {
		systemdDir = rc.SystemdDir
	}
	if rc.DataDir != "" {
		dataDir = rc.DataDir
	}
	hostDataDir := dataDir
	if rc.HostDataDir != "" {
		hostDataDir = rc.HostDataDir
	}
	if err := checkHostDataDir(hostDataDir); err != nil {
		return nil, err
	}
	return &Resolver{
		fsys:           rc.FS,
		cfg:            rc.Config,
		secretReader:   rc.SecretReader,
		opSecretReader: rc.OpSecretReader,
		ppSecretReader: rc.PPSecretReader,
		quadletDir:     quadletDir,
		systemdDir:     systemdDir,
		dataDir:        dataDir,
		hostDataDir:    hostDataDir,
		rootless:       rc.Rootless,
		strict:         rc.Strict,
	}, nil
}

// checkHostDataDir enforces the contract that lets Fleet templates emit
// filePath/manifestPath unquoted into Quadlet, systemd and shell lines: an
// absolute path of letters, digits and . _ - + @ / only. Anything else
// (whitespace, quotes, $ and backticks, systemd's %, Volume='s :) would be
// split or expanded by one of those parsers.
func checkHostDataDir(dir string) error {
	if !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("host data dir %q must be an absolute path", dir)
	}
	for _, r := range dir {
		if !isPlainPathRune(r) {
			return fmt.Errorf("host data dir %q: only letters, digits and . _ - + @ / are allowed; "+
				"filePath and manifestPath emit it unquoted into units and scripts", dir)
		}
	}
	return nil
}

func isPlainPathRune(r rune) bool {
	switch {
	case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		return true
	default:
		return strings.ContainsRune("._-+@/", r)
	}
}

// ResolveDirs computes destination directories based on rootless mode.
// Quadlet files are placed in a picolet-owned subdirectory so that orphan
// detection can safely scan and remove any file in that directory.
func ResolveDirs(rootless bool) (quadletDir, systemdDir, dataDir string, err error) {
	if !rootless {
		return "/etc/containers/systemd/picolet", "/etc/systemd/system", "/var/lib/picolet", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", fmt.Errorf("getting home directory: %w", err)
	}
	return filepath.Join(home, ".config", "containers", "systemd", "picolet"),
		filepath.Join(home, ".config", "systemd", "user"),
		filepath.Join(home, ".local", "share", "picolet"), nil
}

// ResolveHost computes the complete desired state for a given hostname.
func (r *Resolver) ResolveHost(ctx context.Context, hostname string) (*ResolvedHost, error) {
	host, ok := r.cfg.FindHost(hostname)
	if !ok {
		return nil, &HostNotFoundError{Hostname: hostname}
	}
	return r.resolveHostFileSet(ctx, hostname, host, r.cfg.Assignments.Resolve(host), nil)
}

// ResolveServicesForHost resolves only the listed service bundles for a host.
// Host assignment metadata remains the same as a full host resolve, but only
// files and hooks from the requested service bundles are rendered.
func (r *Resolver) ResolveServicesForHost(ctx context.Context, hostname string, services []string) (*ResolvedHost, error) {
	host, ok := r.cfg.FindHost(hostname)
	if !ok {
		return nil, &HostNotFoundError{Hostname: hostname}
	}
	full := r.cfg.Assignments.Resolve(host)
	available := sortedUnique(full.Services)
	requested := sortedUnique(services)
	var missing []string
	for _, service := range requested {
		if !slices.Contains(available, service) {
			missing = append(missing, service)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("service(s) not assigned to host %s: %s (available: %s)",
			hostname, strings.Join(missing, ", "), strings.Join(available, ", "))
	}
	return r.resolveHostFileSet(ctx, hostname, host, &config.ResolvedFileSet{Services: requested}, serviceTemplatePrefixes(requested))
}

func (r *Resolver) resolveHostFileSet(ctx context.Context, hostname string, host *config.HostConfig, fileSet *config.ResolvedFileSet, templatePrefixes []string) (*ResolvedHost, error) {
	tmplData, err := NewTemplateData(r.cfg, hostname)
	if err != nil {
		return nil, err
	}

	providers := []ProviderTemplate{
		r.provider(OpProvider(r.opSecretReader)),
		r.provider(PPProvider(r.ppSecretReader)),
	}
	// hostDataDir (not dataDir) drives filePath/manifestPath: those helpers emit
	// path strings baked into rendered quadlets, which the host's podman must
	// resolve. dataDir remains the write path (dataDestPath).
	registry, caches, err := buildRegistry(ctx, r.fsys, r.secretReader, providers, r.hostDataDir, prefixFilter(templatePrefixes), siblings(tmplData))
	if err != nil {
		return nil, fmt.Errorf("building template registry: %w", err)
	}

	// Fail fast on destination collisions before paying for template rendering
	// or remote secret-provider calls. DestPath is knowable from the file layout alone.
	expanded, err := r.expandAndValidate(fileSet)
	if err != nil {
		return nil, err
	}

	// First render pass: render quadlets with placeholder data to collect
	// secret refs (op://, pass://) for batch resolution and to derive the
	// host's systemd unit names into tmplData.Host.SystemdUnits.
	if _, err := r.prepareTemplateData(ctx, registry, tmplData, expanded, caches); err != nil {
		return nil, err
	}

	// Batch-resolve direct (non-template) secret refs in one call per provider.
	resolvedDirect, err := r.batchResolveDirectSecrets(ctx, secretSources(expanded.Files))
	if err != nil {
		return nil, err
	}

	files, err := r.buildFiles(registry, tmplData, expanded.Files, resolvedDirect)
	if err != nil {
		return nil, err
	}
	hooks, err := r.buildHooks(registry, tmplData, expanded.Hooks, files)
	if err != nil {
		return nil, err
	}

	return &ResolvedHost{
		Hostname:    hostname,
		Host:        host,
		Files:       files,
		Hooks:       hooks,
		DataDir:     r.dataDir,
		HostDataDir: r.hostDataDir,
	}, nil
}

func (r *Resolver) provider(p ProviderTemplate) ProviderTemplate {
	p.Strict = r.strict
	return p
}

func serviceTemplatePrefixes(services []string) []string {
	prefixes := make([]string, 0, len(services))
	for _, service := range services {
		prefixes = append(prefixes, path.Join("services", service)+"/")
	}
	return prefixes
}

func prefixFilter(prefixes []string) func(string) bool {
	if len(prefixes) == 0 {
		return nil
	}
	return func(path string) bool {
		return slices.ContainsFunc(prefixes, func(prefix string) bool {
			return strings.HasPrefix(path, prefix)
		})
	}
}

// preparedData is the output of the first render pass.
//
// Some template data only becomes knowable AFTER quadlet templates render:
//   - Secret refs (op://, pass://) — providers batch better when picolet knows
//     all refs upfront, so we collect them first and resolve in bulk.
//   - Systemd unit names — a quadlet's unit name is derived from its rendered
//     content via Podman's parser, so .Host.SystemdUnits can only be populated
//     AFTER every quadlet has been rendered at least once.
//
// The first pass renders every .tmpl with placeholder data (nil/empty for the
// not-yet-discovered fields). Quadlet renders are parsed to harvest unit names;
// other renders are kept only for their secret-collection side effect. The
// second (final) pass renders for real with fully populated TemplateData.
// Render errors in the first pass are non-fatal; the final pass surfaces real
// template errors with proper diagnostics.
//
// Cost: quadlet templates render once in the first pass for unit discovery,
// plus a second first-pass render for secret collection when secret providers
// are configured, and once more in the final pass. All renders are in-memory;
// cost is negligible vs. git fetches and secret-provider round-trips.
type preparedData struct {
	SystemdUnits []string // sorted, unique; e.g. "node-exporter.service"
}

// prepareTemplateData runs the first render pass: it derives the host's systemd
// unit names, collects secret refs for any configured providers, and resolves
// each provider's cache. It populates tmplData.Host.SystemdUnits as a side
// effect and also returns the result so the pass is testable in isolation.
func (r *Resolver) prepareTemplateData(ctx context.Context, registry *template.Template, tmplData *TemplateData, expanded *expansion, caches ProviderCaches) (*preparedData, error) {
	units := r.collectSystemdUnits(registry, tmplData, expanded.Files)
	if len(caches) > 0 {
		r.collectTemplateRefs(registry, tmplData, expanded)
		if err := caches.ResolveAll(ctx); err != nil {
			return nil, err
		}
	}
	tmplData.Host.SystemdUnits = units
	return &preparedData{SystemdUnits: units}, nil
}

// collectSystemdUnits derives the sorted, unique list of systemd unit names
// picolet manages on the host. Quadlet units (GeneratedUnit categories) are
// rendered and parsed via Podman's GetUnitServiceName, which honors
// ServiceName= overrides; FileUnit categories contribute their filename with
// any .tmpl suffix stripped. Render and parse errors are swallowed here — the
// final pass and the validator surface them with proper diagnostics.
func (r *Resolver) collectSystemdUnits(registry *template.Template, tmplData *TemplateData, refs []fileRef) []string {
	var units []string
	for _, ref := range refs {
		spec, _ := config.SpecFor(ref.Category)
		switch spec.Unit {
		case config.FileUnit:
			units = append(units, destFilename(ref.SrcPath))
		case config.GeneratedUnit:
			f, err := r.resolveFile(registry, tmplData, ref.SrcPath, spec, r.unitDestPath(spec, ref.SrcPath))
			if err == nil && f.ServiceName != "" {
				units = append(units, f.ServiceName)
			}
		case config.NoUnit:
		}
	}
	return sortedUnique(units)
}

// ResolveAll resolves all hosts and returns the results.
func (r *Resolver) ResolveAll(ctx context.Context) (map[string]*ResolvedHost, error) {
	results := make(map[string]*ResolvedHost, len(r.cfg.Hosts))
	for _, hostname := range r.cfg.SortedHostnames() {
		resolved, err := r.ResolveHost(ctx, hostname)
		if err != nil {
			return nil, fmt.Errorf("resolving host %s: %w", hostname, err)
		}
		results[hostname] = resolved
	}
	return results, nil
}

// expandAndValidate expands the host's assignment entries into categorized
// files and fails fast if any two sources resolve to the same destination.
func (r *Resolver) expandAndValidate(fileSet *config.ResolvedFileSet) (*expansion, error) {
	expanded, err := r.expandFileSet(fileSet)
	if err != nil {
		return nil, err
	}
	skeletons, err := r.buildFileSkeletons(expanded.Files)
	if err != nil {
		return nil, err
	}
	if err := detectCollisions(skeletons); err != nil {
		return nil, err
	}
	return expanded, nil
}

// expandFileSet expands service bundles and `paths:` entries and adds the
// `secrets:` entries, which are not expanded: they may name host-only
// secrets or provider refs rather than repo files. Files are returned in
// resolution order, each once.
func (r *Resolver) expandFileSet(fileSet *config.ResolvedFileSet) (*expansion, error) {
	expanded, bundleErr := expandServiceBundles(r.fsys, fileSet.Services)
	fromPaths, pathsErr := expandPathEntries(r.fsys, fileSet.Paths)
	if err := errors.Join(bundleErr, pathsErr); err != nil {
		return nil, err
	}
	expanded.append(fromPaths)
	for _, src := range fileSet.Secrets {
		expanded.Files = append(expanded.Files, fileRef{SrcPath: src, Category: config.CategorySecret})
	}
	expanded.Files = uniqueFileRefs(expanded.Files)
	return expanded, nil
}

// secretSources returns the source of every secret-category ref.
func secretSources(refs []fileRef) []string {
	var srcs []string
	for _, ref := range refs {
		if ref.Category == config.CategorySecret {
			srcs = append(srcs, ref.SrcPath)
		}
	}
	return srcs
}

// buildFileSkeletons returns SrcPath/Category/DestPath tuples for every file
// the host will deploy. It does not render templates, read files, or call the
// 1Password SDK, so it's safe (and cheap) to run before expensive operations.
func (r *Resolver) buildFileSkeletons(refs []fileRef) ([]ResolvedFile, error) {
	skeletons := make([]ResolvedFile, 0, len(refs))
	for _, ref := range refs {
		dest, err := r.destPath(ref)
		if err != nil {
			return nil, fmt.Errorf("resolving %s %s: %w", ref.Category, ref.SrcPath, err)
		}
		skeletons = append(skeletons, ResolvedFile{
			SrcPath: ref.SrcPath, Category: ref.Category, DestPath: dest, RelPath: ref.RelPath,
		})
	}
	return skeletons, nil
}

// unitDestPath returns the deployed path of a Quadlet or raw systemd source.
func (r *Resolver) unitDestPath(spec config.Spec, srcPath string) string {
	dir := r.quadletDir
	if spec.Dest == config.DestSystemd {
		dir = r.systemdDir
	}
	return filepath.Join(dir, destFilename(srcPath))
}

// destPath returns where ref deploys.
func (r *Resolver) destPath(ref fileRef) (string, error) {
	spec, _ := config.SpecFor(ref.Category)
	switch spec.Dest {
	case config.DestData:
		return r.dataDestPath(ref.DataPath), nil
	case config.DestSecret:
		return r.secretDestPath(ref.SrcPath)
	case config.DestQuadlet, config.DestSystemd:
		return r.unitDestPath(spec, ref.SrcPath), nil
	}
	return "", fmt.Errorf("category %s: unknown destination %d", spec.Category, spec.Dest)
}

func (r *Resolver) dataDestPath(logicalPath string) string {
	return filepath.Join(r.dataDir, filepath.FromSlash(deployedLogicalPath(logicalPath)))
}

func deployedLogicalPath(logicalPath string) string {
	return strings.TrimSuffix(logicalPath, ".tmpl")
}

// secretDestPath returns the DestPath for either a provider-backed ref
// (op:// or pass://) or a file-based secret. Parsing is pure — no I/O.
func (r *Resolver) secretDestPath(srcPath string) (string, error) {
	if op.IsRef(srcPath) {
		parsed, err := op.ParseOpRef(srcPath)
		if err != nil {
			return "", err
		}
		return "secret:" + parsed.PodmanSecretName(), nil
	}
	if pp.IsRef(srcPath) {
		parsed, err := pp.ParseRef(srcPath)
		if err != nil {
			return "", err
		}
		return "secret:" + parsed.PodmanSecretName(), nil
	}
	filename := destFilename(srcPath)
	return "secret:" + strings.TrimSuffix(filename, filepath.Ext(filename)), nil
}

// buildFiles renders or reads every ref, in resolution order. A provider ref
// whose provider is not configured is skipped (batchResolveDirectSecrets).
func (r *Resolver) buildFiles(
	registry *template.Template,
	tmplData *TemplateData,
	refs []fileRef,
	resolvedDirect map[string]string,
) ([]ResolvedFile, error) {
	files := make([]ResolvedFile, 0, len(refs))
	for _, ref := range refs {
		spec, _ := config.SpecFor(ref.Category)
		var (
			f   *ResolvedFile
			err error
		)
		switch spec.Dest {
		case config.DestQuadlet, config.DestSystemd:
			f, err = r.resolveFile(registry, tmplData, ref.SrcPath, spec, r.unitDestPath(spec, ref.SrcPath))
		case config.DestData:
			f, err = r.resolveNestedRef(registry, tmplData, ref)
		case config.DestSecret:
			if isProviderRef(ref.SrcPath) {
				if resolvedDirect == nil {
					continue
				}
				f, err = r.buildDirectSecretFile(ref.SrcPath, resolvedDirect[ref.SrcPath])
			} else {
				f, err = r.resolveSecret(registry, tmplData, ref.SrcPath)
			}
		}
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}
	return files, nil
}

// isProviderRef reports whether a secrets entry is a provider ref (op://,
// pass://) rather than a repo or host-only secret file.
func isProviderRef(src string) bool {
	return op.IsRef(src) || pp.IsRef(src)
}

func detectCollisions(files []ResolvedFile) error {
	collisions := make(map[string][]string)
	for _, file := range files {
		collisions[file.DestPath] = append(collisions[file.DestPath], file.SrcPath)
	}

	destPaths := make([]string, 0, len(collisions))
	for destPath := range collisions {
		destPaths = append(destPaths, destPath)
	}
	slices.Sort(destPaths)

	var errs []error
	for _, destPath := range destPaths {
		uniquePaths := sortedUnique(collisions[destPath])
		if len(uniquePaths) < 2 {
			continue
		}
		errs = append(errs, fmt.Errorf("destination collision for %s: %s", destPath, strings.Join(uniquePaths, ", ")))
	}
	return errors.Join(errs...)
}

func (r *Resolver) resolveFile(registry *template.Template, tmplData *TemplateData, srcPath string, spec config.Spec, destPath string) (*ResolvedFile, error) {
	content, err := r.renderOrRead(registry, tmplData, srcPath)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", srcPath, err)
	}

	if spec.Dest == config.DestSystemd {
		content = config.PicoletMarker + "\n" + content
	}

	filename := destFilename(srcPath)
	var parsedUnit *parser.UnitFile
	var serviceName string
	switch spec.Unit {
	case config.GeneratedUnit:
		unit := parser.NewUnitFile()
		unit.Filename = filename
		if err := unit.Parse(content); err == nil {
			parsedUnit = unit
			serviceName = unitServiceName(unit)
		}
		// Parse errors are silent here — validator catches them with proper error messages
	case config.FileUnit:
		// Raw systemd units are not parsed; the unit name is the filename. Populate
		// it so the unit is tracked in state.ServiceNames — driving health checks,
		// the status store, and the dashboard, just like quadlet-derived units.
		serviceName = filename
	case config.NoUnit:
	}

	return &ResolvedFile{
		SrcPath:     srcPath,
		DestPath:    destPath,
		Content:     content,
		Category:    spec.Category,
		ParsedUnit:  parsedUnit,
		ServiceName: serviceName,
	}, nil
}

// unitServiceName returns "foo.service" from a parsed quadlet unit, using Podman's
// GetUnitServiceName which handles all quadlet types and ServiceName= overrides.
func unitServiceName(unit *parser.UnitFile) string {
	name, err := quadlet.GetUnitServiceName(unit)
	if err != nil {
		return ""
	}
	return name + ".service"
}

// findQuadletFile returns the ResolvedFile whose destination filename matches
// quadletName, or nil if none. Quadlet filename is the source basename minus
// any .tmpl suffix (e.g. "app.container").
func findQuadletFile(quadletName string, files []ResolvedFile) *ResolvedFile {
	for i := range files {
		if destFilename(files[i].SrcPath) == quadletName {
			return &files[i]
		}
	}
	return nil
}

// validateSignalHookContainer cross-checks a signal-action hook's Container
// against the Quadlet [Container] ContainerName= when the unit is
// Quadlet-resolvable and the field is explicitly set. Must be called BEFORE
// resolveHookQuadletUnit rewrites hook.Unit, since the lookup compares against
// the original Quadlet filename (e.g. "app.container").
func validateSignalHookContainer(hook config.Hook, files []ResolvedFile) error {
	if hook.Action != config.HookActionSignal || !isQuadletUnit(hook.Unit) {
		return nil
	}
	file := findQuadletFile(hook.Unit, files)
	if file == nil || file.ParsedUnit == nil {
		return nil
	}
	declared, ok := file.ParsedUnit.LookupLast("Container", "ContainerName")
	declared = strings.TrimSpace(declared)
	if !ok || declared == "" {
		slog.Debug("Quadlet has no explicit ContainerName, container not validated",
			"hook", hook.Name, "unit", hook.Unit, "container", hook.Container)
		return nil
	}
	if declared != hook.Container {
		return fmt.Errorf("hook %s: container %q does not match Quadlet ContainerName %q for unit %s",
			hook.Name, hook.Container, declared, hook.Unit)
	}
	return nil
}

// resolveHookQuadletUnit finds the ResolvedFile matching the given Quadlet filename
// and returns its ServiceName (computed by the Podman library via GetUnitServiceName).
// Returns an error if no matching file exists in the resolved set.
func resolveHookQuadletUnit(quadletName string, files []ResolvedFile) (string, error) {
	if file := findQuadletFile(quadletName, files); file != nil && file.ServiceName != "" {
		return file.ServiceName, nil
	}
	return "", fmt.Errorf("unit %q: no matching quadlet file found in assigned bundles", quadletName)
}

// isQuadletUnit reports whether the given unit name has a Quadlet file extension,
// indicating it needs resolution to its generated systemd service name.
func isQuadletUnit(unit string) bool {
	category, ok := config.CategoryForExtension(filepath.Ext(unit))
	if !ok {
		return false
	}
	spec, _ := config.SpecFor(category)
	return spec.Dest == config.DestQuadlet
}

// destFilename returns the base filename for a source path, stripping any .tmpl suffix.
func destFilename(srcPath string) string {
	return strings.TrimSuffix(path.Base(srcPath), ".tmpl")
}

func (r *Resolver) resolveNestedRef(registry *template.Template, tmplData *TemplateData, ref fileRef) (*ResolvedFile, error) {
	content, err := r.renderOrRead(registry, tmplData, ref.SrcPath)
	if err != nil {
		return nil, fmt.Errorf("resolving %s %s: %w", ref.Category, ref.SrcPath, err)
	}

	return &ResolvedFile{
		SrcPath:  ref.SrcPath,
		DestPath: r.dataDestPath(ref.DataPath),
		Content:  content,
		Category: ref.Category,
		RelPath:  ref.RelPath,
	}, nil
}

func (r *Resolver) buildHooks(registry *template.Template, tmplData *TemplateData, refs []hookRef, files []ResolvedFile) ([]config.Hook, error) {
	type hookOrigin struct {
		service string
		path    string
	}
	var hooks []config.Hook
	seen := make(map[string]hookOrigin)
	for _, ref := range refs {
		fileHooks, err := r.resolveHooksFile(registry, tmplData, ref, files)
		if err != nil {
			return nil, err
		}
		for _, hook := range fileHooks {
			if prev, ok := seen[hook.Name]; ok {
				return nil, fmt.Errorf("%s: duplicate hook name %q (already defined by service %q in %s)", ref.SrcPath, hook.Name, prev.service, prev.path)
			}
			seen[hook.Name] = hookOrigin{service: ref.Service, path: ref.SrcPath}
			hooks = append(hooks, hook)
		}
	}
	return hooks, nil
}

func (r *Resolver) resolveHooksFile(registry *template.Template, tmplData *TemplateData, ref hookRef, files []ResolvedFile) ([]config.Hook, error) {
	content, err := r.renderOrRead(registry, tmplData, ref.SrcPath)
	if err != nil {
		return nil, fmt.Errorf("resolving hooks %s: %w", ref.SrcPath, err)
	}
	var file config.HooksFile
	if err := yaml.Load([]byte(content), &file, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", ref.SrcPath, err)
	}
	for i := range file.Hooks {
		if err := validateSignalHookContainer(file.Hooks[i], files); err != nil {
			return nil, fmt.Errorf("%s: hooks[%d]: %w", ref.SrcPath, i, err)
		}
		if isQuadletUnit(file.Hooks[i].Unit) {
			resolved, err := resolveHookQuadletUnit(file.Hooks[i].Unit, files)
			if err != nil {
				return nil, fmt.Errorf("%s: hooks[%d]: %w", ref.SrcPath, i, err)
			}
			file.Hooks[i].Unit = resolved
		}
		if err := file.Hooks[i].Normalize(); err != nil {
			return nil, fmt.Errorf("%s: hooks[%d]: %w", ref.SrcPath, i, err)
		}
	}
	return file.Hooks, nil
}

func (r *Resolver) resolveSecret(registry *template.Template, tmplData *TemplateData, srcPath string) (*ResolvedFile, error) {
	filename := destFilename(srcPath)
	content, err := r.secretContent(registry, tmplData, srcPath, filename)
	if err != nil {
		return nil, fmt.Errorf("resolving secret %s: %w", srcPath, err)
	}
	destPath, err := r.secretDestPath(srcPath)
	if err != nil {
		return nil, fmt.Errorf("resolving secret %s: %w", srcPath, err)
	}

	return &ResolvedFile{
		SrcPath:  srcPath,
		DestPath: destPath,
		Content:  content,
		Category: config.CategorySecret,
	}, nil
}

// batchResolveDirectSecrets resolves all provider-backed (op:// + pass://)
// refs in the secrets list, one batched call per provider. Returns a single
// map keyed by ref. A resolution failure is fatal to prevent reconciler.Diff
// from marking unresolved secrets for deletion (which would remove them from
// Podman). When a provider is not configured, its refs are skipped with a
// warning so the rest of the reconcile can proceed.
//
//nolint:nilnil // nil map signals "no provider-backed secrets to resolve"
func (r *Resolver) batchResolveDirectSecrets(ctx context.Context, allSecrets []string) (map[string]string, error) {
	opRefs, ppRefs := splitDirectRefs(allSecrets)
	if len(opRefs) == 0 && len(ppRefs) == 0 {
		return nil, nil
	}

	results := make(map[string]string)
	if err := r.resolveProviderRefs(ctx, ProviderOnePassword, r.opSecretReader, opRefs, results); err != nil {
		return nil, err
	}
	if err := r.resolveProviderRefs(ctx, ProviderProtonPass, r.ppSecretReader, ppRefs, results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results, nil
}

func splitDirectRefs(allSecrets []string) (opRefs, ppRefs []string) {
	for _, path := range allSecrets {
		switch {
		case op.IsRef(path):
			opRefs = append(opRefs, path)
		case pp.IsRef(path):
			ppRefs = append(ppRefs, path)
		}
	}
	return opRefs, ppRefs
}

func (r *Resolver) resolveProviderRefs(ctx context.Context, name ProviderKey, reader SecretRefReader, refs []string, into map[string]string) error {
	if len(refs) == 0 {
		return nil
	}
	if reader == nil {
		if r.strict {
			return fmt.Errorf("%s provider not configured; cannot resolve direct secrets in strict mode: %s", name, strings.Join(refs, ", "))
		}
		slog.Warn("skipping secrets (provider not configured)", "provider", name, "count", len(refs))
		return nil
	}
	slog.Debug("batch-resolving secrets", "provider", name, "count", len(refs))
	results, err := reader(ctx, refs)
	if err != nil {
		return fmt.Errorf("resolving %s secrets: %w", name, err)
	}
	maps.Copy(into, results)
	return nil
}

// collectTemplateRefs executes all .tmpl files in collect mode to discover
// reader-function calls (readOpSecret, readProtonPassSecret, …). Output is
// discarded — only the side effect of populating each provider's RefCache matters.
// Secret templates are included: they may call provider reader functions;
// direct provider refs (op://, pass://) are not templates and are skipped.
func (r *Resolver) collectTemplateRefs(registry *template.Template, tmplData *TemplateData, expanded *expansion) {
	allPaths := make([]string, 0, len(expanded.Files)+len(expanded.Hooks))
	for _, ref := range expanded.Files {
		if !isProviderRef(ref.SrcPath) {
			allPaths = append(allPaths, ref.SrcPath)
		}
	}
	for _, ref := range expanded.Hooks {
		allPaths = append(allPaths, ref.SrcPath)
	}
	for _, path := range allPaths {
		if !strings.HasSuffix(path, ".tmpl") {
			continue
		}
		_ = registry.ExecuteTemplate(io.Discard, path, tmplData) // errors are non-fatal in collect phase
	}
}

// buildDirectSecretFile creates a ResolvedFile for a pre-resolved
// provider-backed secret (op:// or pass://).
func (r *Resolver) buildDirectSecretFile(ref, content string) (*ResolvedFile, error) {
	destPath, err := r.secretDestPath(ref)
	if err != nil {
		return nil, err
	}
	return &ResolvedFile{
		SrcPath:  ref,
		DestPath: destPath,
		Content:  content,
		Category: config.CategorySecret,
	}, nil
}

// secretContent returns the content for a secret entry.
// Modes, in priority order:
//  1. Template secrets (.tmpl suffix) are rendered with the full template engine.
//  2. Static repo secrets (file exists in repo without .tmpl) are copied as-is.
//  3. Host-only secrets (not in repo) are read from SecretsDir via secretReader.
//  4. If no secretReader is configured, a placeholder value ("<secret>") is returned and a warning is logged.
func (r *Resolver) secretContent(registry *template.Template, tmplData *TemplateData, srcPath, filename string) (string, error) {
	if strings.HasSuffix(srcPath, ".tmpl") {
		return r.renderOrRead(registry, tmplData, srcPath)
	}
	// Static repo file — copy as-is without template rendering.
	data, readErr := fs.ReadFile(r.fsys, srcPath)
	if readErr == nil {
		slog.Debug("reading static secret from repo", "path", srcPath)
		return string(data), nil
	}
	if !errors.Is(readErr, fs.ErrNotExist) {
		return "", fmt.Errorf("reading static secret %s: %w", srcPath, readErr)
	}
	// Host-only secret (API keys, tokens) — read from SecretsDir.
	if r.secretReader != nil {
		return r.secretReader(filename)
	}
	slog.Warn("secret reader not configured, using placeholder", "file", srcPath)
	return placeholderSecret, nil
}

func (r *Resolver) renderOrRead(registry *template.Template, tmplData *TemplateData, path string) (string, error) {
	if strings.HasSuffix(path, ".tmpl") {
		slog.Debug("rendering template", "path", path)
		var buf bytes.Buffer
		if err := registry.ExecuteTemplate(&buf, path, tmplData); err != nil {
			return "", fmt.Errorf("executing template %s: %w", path, err)
		}
		return buf.String(), nil
	}

	slog.Debug("reading static file", "path", path)
	data, err := fs.ReadFile(r.fsys, path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return string(data), nil
}
