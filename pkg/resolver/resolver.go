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

// PicoletMarker is the comment header prepended to systemd unit files managed by picolet.
// Including it in Content (and thus in the state hash) ensures orphan detection works
// correctly: a one-time ActionUpdate rewrites any pre-existing file with the marker,
// after which the hash remains stable.
const PicoletMarker = "# Managed by picolet"

// ResolvedFile represents a single rendered file with its destination path.
type ResolvedFile struct {
	// SrcPath is the source template/file path within the repo.
	SrcPath string
	// DestPath is where the file should be deployed on the target host.
	DestPath string
	// Content is the rendered file content.
	Content string
	// Category describes the file type (network, container, kube, manifest, etc.).
	Category string
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
}

// Config holds configuration for creating a Resolver.
type Config struct {
	FS             fs.FS
	Config         *config.Config
	SecretReader   SecretReader
	OpSecretReader SecretRefReader
	PPSecretReader SecretRefReader
	Rootless       bool

	// QuadletDir, SystemdDir, and DataDir override the defaults computed by
	// ResolveDirs. Empty fields fall back to the default for the given
	// Rootless mode. Used by tests to isolate destination paths from a
	// shared host filesystem; production callers leave them empty.
	QuadletDir string
	SystemdDir string
	DataDir    string
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
	rootless       bool
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
	return &Resolver{
		fsys:           rc.FS,
		cfg:            rc.Config,
		secretReader:   rc.SecretReader,
		opSecretReader: rc.OpSecretReader,
		ppSecretReader: rc.PPSecretReader,
		quadletDir:     quadletDir,
		systemdDir:     systemdDir,
		dataDir:        dataDir,
		rootless:       rc.Rootless,
	}, nil
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
	host, ok := r.cfg.Hosts[hostname]
	if !ok {
		return nil, &HostNotFoundError{Hostname: hostname}
	}

	tmplData, err := NewTemplateData(r.cfg, hostname)
	if err != nil {
		return nil, err
	}

	providers := []ProviderTemplate{
		OpProvider(r.opSecretReader),
		PPProvider(r.ppSecretReader),
	}
	registry, caches, err := BuildRegistry(ctx, r.fsys, r.secretReader, providers, r.dataDir)
	if err != nil {
		return nil, fmt.Errorf("building template registry: %w", err)
	}

	// Fail fast on destination collisions before paying for template rendering
	// or remote secret-provider calls. DestPath is knowable from the file layout alone.
	expanded, err := r.expandAndValidate(r.cfg.Assignments.Resolve(host))
	if err != nil {
		return nil, err
	}

	// Two-phase secret resolution for templates: collect refs by rendering
	// once with placeholders, then batch-resolve, then render for real. Each
	// provider has its own cache; rendering twice is cheap relative to the
	// network/exec round-trips we save.
	if err := r.runTemplateRefCollection(ctx, registry, tmplData, expanded, caches); err != nil {
		return nil, err
	}

	// Batch-resolve direct (non-template) secret refs in one call per provider.
	resolvedDirect, err := r.batchResolveDirectSecrets(ctx, expanded.FileSet.Secrets)
	if err != nil {
		return nil, err
	}

	files, err := r.buildFiles(registry, tmplData, expanded.FileSet, expanded.ManifestRefs, resolvedDirect)
	if err != nil {
		return nil, err
	}
	hooks, err := r.buildHooks(registry, tmplData, expanded.HookRefs, files)
	if err != nil {
		return nil, err
	}

	return &ResolvedHost{
		Hostname: hostname,
		Host:     host,
		Files:    files,
		Hooks:    hooks,
	}, nil
}

// runTemplateRefCollection executes the collect pass for any configured
// providers and resolves each provider's cache. Pulled out of ResolveHost to
// keep ResolveHost under the cyclop limit.
func (r *Resolver) runTemplateRefCollection(ctx context.Context, registry *template.Template, tmplData *TemplateData, expanded *expandedResult, caches ProviderCaches) error {
	if len(caches) == 0 {
		return nil
	}
	r.collectTemplateRefs(registry, tmplData, expanded.FileSet, expanded.ManifestRefs, expanded.HookRefs)
	return caches.ResolveAll(ctx)
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

// expandedResult holds the outputs of bundle expansion and validation.
type expandedResult struct {
	FileSet      *config.ResolvedFileSet
	ManifestRefs []bundleFileRef
	HookRefs     []hookRef
}

// expandAndValidate expands service bundles into the file set and fails fast
// if any two sources resolve to the same destination path.
func (r *Resolver) expandAndValidate(fileSet *config.ResolvedFileSet) (*expandedResult, error) {
	merged, manifestRefs, hookRefs, err := r.expandFileSet(fileSet)
	if err != nil {
		return nil, err
	}
	skeletons, err := r.buildFileSkeletons(merged, manifestRefs)
	if err != nil {
		return nil, err
	}
	if err := detectCollisions(skeletons); err != nil {
		return nil, err
	}
	return &expandedResult{
		FileSet:      merged,
		ManifestRefs: manifestRefs,
		HookRefs:     hookRefs,
	}, nil
}

// expandFileSet returns a new ResolvedFileSet merged with any service bundles,
// plus the full list of manifest refs (legacy + bundled). The input fileSet is
// not mutated. Manifests and Services are left nil: manifest paths flow through
// manifestRefs, and Services is already flattened into the category slices.
// Populating them would let a future caller miss bundle contents.
func (r *Resolver) expandFileSet(fileSet *config.ResolvedFileSet) (*config.ResolvedFileSet, []bundleFileRef, []hookRef, error) {
	expanded, err := expandServiceBundles(r.fsys, fileSet.Services)
	if err != nil {
		return nil, nil, nil, err
	}

	merged := &config.ResolvedFileSet{
		Networks:   sortedUnique(slices.Concat(fileSet.Networks, expanded.Networks)),
		Systemd:    sortedUnique(slices.Concat(fileSet.Systemd, expanded.Systemd)),
		Volumes:    sortedUnique(slices.Concat(fileSet.Volumes, expanded.Volumes)),
		Containers: sortedUnique(slices.Concat(fileSet.Containers, expanded.Containers)),
		Kube:       sortedUnique(slices.Concat(fileSet.Kube, expanded.Kube)),
		Secrets:    sortedUnique(slices.Concat(fileSet.Secrets, expanded.Secrets)),
	}

	manifestRefs := make([]bundleFileRef, 0, len(fileSet.Manifests)+len(fileSet.Files)+len(expanded.NestedRefs))
	for _, srcPath := range fileSet.Manifests {
		manifestRefs = append(manifestRefs, newLegacyManifestRef(srcPath))
	}
	for _, srcPath := range fileSet.Files {
		manifestRefs = append(manifestRefs, bundleFileRef{
			SrcPath:     srcPath,
			LogicalPath: srcPath,
			Category:    "file",
			RelPath:     stripCategoryPrefix(srcPath, "files"),
		})
	}
	manifestRefs = append(manifestRefs, expanded.NestedRefs...)
	return merged, uniqueManifestRefs(manifestRefs), expanded.Hooks, nil
}

// newLegacyManifestRef constructs a bundleFileRef for a legacy (non-bundled)
// manifest path, where the source and logical paths are the same. Bundled
// refs set a stripped LogicalPath and are built in readNestedSubdir.
func newLegacyManifestRef(srcPath string) bundleFileRef {
	return bundleFileRef{
		SrcPath:     srcPath,
		LogicalPath: srcPath,
		Category:    "manifest",
		RelPath:     stripCategoryPrefix(srcPath, "manifests"),
	}
}

// buildFileSkeletons returns SrcPath/Category/DestPath tuples for every file
// the host will deploy. It does not render templates, read files, or call the
// 1Password SDK, so it's safe (and cheap) to run before expensive operations.
func (r *Resolver) buildFileSkeletons(fileSet *config.ResolvedFileSet, manifestRefs []bundleFileRef) ([]ResolvedFile, error) {
	total := len(fileSet.Networks) + len(fileSet.Systemd) + len(fileSet.Volumes) +
		len(fileSet.Containers) + len(fileSet.Kube) + len(manifestRefs) + len(fileSet.Secrets)
	skeletons := make([]ResolvedFile, 0, total)

	quadletCats := []struct {
		category string
		paths    []string
	}{
		{"network", fileSet.Networks},
		{"volume", fileSet.Volumes},
		{"container", fileSet.Containers},
		{"kube", fileSet.Kube},
	}
	for _, g := range quadletCats {
		for _, srcPath := range g.paths {
			skeletons = append(skeletons, ResolvedFile{
				SrcPath: srcPath, Category: g.category, DestPath: r.quadletDestPath(srcPath),
			})
		}
	}
	for _, srcPath := range fileSet.Systemd {
		skeletons = append(skeletons, ResolvedFile{
			SrcPath: srcPath, Category: "systemd", DestPath: r.systemdDestPath(srcPath),
		})
	}
	for _, ref := range manifestRefs {
		skeletons = append(skeletons, ResolvedFile{
			SrcPath: ref.SrcPath, Category: "manifest", DestPath: r.dataDestPath(ref.LogicalPath),
			RelPath: ref.RelPath,
		})
	}
	for _, srcPath := range fileSet.Secrets {
		dest, err := r.secretDestPath(srcPath)
		if err != nil {
			return nil, fmt.Errorf("resolving secret %s: %w", srcPath, err)
		}
		skeletons = append(skeletons, ResolvedFile{
			SrcPath: srcPath, Category: "secret", DestPath: dest,
		})
	}
	return skeletons, nil
}

func (r *Resolver) quadletDestPath(srcPath string) string {
	return filepath.Join(r.quadletDir, destFilename(srcPath))
}

func (r *Resolver) systemdDestPath(srcPath string) string {
	return filepath.Join(r.systemdDir, destFilename(srcPath))
}

func (r *Resolver) dataDestPath(logicalPath string) string {
	return filepath.Join(r.dataDir, filepath.FromSlash(strings.TrimSuffix(logicalPath, ".tmpl")))
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

func uniqueManifestRefs(refs []bundleFileRef) []bundleFileRef {
	slices.SortFunc(refs, func(a, b bundleFileRef) int {
		if diff := strings.Compare(a.LogicalPath, b.LogicalPath); diff != 0 {
			return diff
		}
		return strings.Compare(a.SrcPath, b.SrcPath)
	})
	return slices.CompactFunc(refs, func(a, b bundleFileRef) bool {
		return a == b
	})
}

func (r *Resolver) buildFiles(
	registry *template.Template,
	tmplData *TemplateData,
	fileSet *config.ResolvedFileSet,
	manifestRefs []bundleFileRef,
	opResolved map[string]string,
) ([]ResolvedFile, error) {
	var files []ResolvedFile

	standardFiles, err := r.buildStandardFiles(registry, tmplData, fileSet)
	if err != nil {
		return nil, err
	}
	files = append(files, standardFiles...)

	for _, ref := range manifestRefs {
		f, err := r.resolveNestedRef(registry, tmplData, ref)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}

	secretFiles, err := r.buildSecretFiles(registry, tmplData, fileSet.Secrets, opResolved)
	if err != nil {
		return nil, err
	}
	files = append(files, secretFiles...)

	return files, nil
}

func (r *Resolver) buildStandardFiles(
	registry *template.Template,
	tmplData *TemplateData,
	fileSet *config.ResolvedFileSet,
) ([]ResolvedFile, error) {
	fileGroups := []struct {
		paths    []string
		cat      string
		destPath func(string) string
		quadlet  bool
	}{
		{fileSet.Networks, "network", r.quadletDestPath, true},
		{fileSet.Systemd, "systemd", r.systemdDestPath, false},
		{fileSet.Volumes, "volume", r.quadletDestPath, true},
		{fileSet.Containers, "container", r.quadletDestPath, true},
		{fileSet.Kube, "kube", r.quadletDestPath, true},
	}

	var files []ResolvedFile
	for _, g := range fileGroups {
		for _, srcPath := range g.paths {
			f, err := r.resolveFile(registry, tmplData, srcPath, g.cat, g.destPath(srcPath), g.quadlet)
			if err != nil {
				return nil, err
			}
			files = append(files, *f)
		}
	}
	return files, nil
}

func (r *Resolver) buildSecretFiles(
	registry *template.Template,
	tmplData *TemplateData,
	secrets []string,
	resolvedDirect map[string]string,
) ([]ResolvedFile, error) {
	var files []ResolvedFile
	for _, srcPath := range secrets {
		if op.IsRef(srcPath) || pp.IsRef(srcPath) {
			if resolvedDirect == nil {
				continue
			}
			f, err := r.buildDirectSecretFile(srcPath, resolvedDirect[srcPath])
			if err != nil {
				return nil, err
			}
			files = append(files, *f)
			continue
		}

		f, err := r.resolveSecret(registry, tmplData, srcPath)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}
	return files, nil
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

func (r *Resolver) resolveFile(registry *template.Template, tmplData *TemplateData, srcPath, category, destPath string, quadlet bool) (*ResolvedFile, error) {
	content, err := r.renderOrRead(registry, tmplData, srcPath)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", srcPath, err)
	}

	if !quadlet && category == "systemd" {
		content = PicoletMarker + "\n" + content
	}

	filename := destFilename(srcPath)
	var parsedUnit *parser.UnitFile
	var serviceName string
	if quadlet {
		unit := parser.NewUnitFile()
		unit.Filename = filename
		if err := unit.Parse(content); err == nil {
			parsedUnit = unit
			serviceName = unitServiceName(unit)
		}
		// Parse errors are silent here — validator catches them with proper error messages
	}

	return &ResolvedFile{
		SrcPath:     srcPath,
		DestPath:    destPath,
		Content:     content,
		Category:    category,
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
	ext := filepath.Ext(unit)
	_, ok := quadlet.SupportedExtensions[ext]
	return ok
}

// destFilename returns the base filename for a source path, stripping any .tmpl suffix.
func destFilename(srcPath string) string {
	return strings.TrimSuffix(path.Base(srcPath), ".tmpl")
}

func (r *Resolver) resolveNestedRef(registry *template.Template, tmplData *TemplateData, ref bundleFileRef) (*ResolvedFile, error) {
	content, err := r.renderOrRead(registry, tmplData, ref.SrcPath)
	if err != nil {
		return nil, fmt.Errorf("resolving %s %s: %w", ref.Category, ref.SrcPath, err)
	}

	return &ResolvedFile{
		SrcPath:  ref.SrcPath,
		DestPath: r.dataDestPath(ref.LogicalPath),
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
		Category: "secret",
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
func (r *Resolver) collectTemplateRefs(
	registry *template.Template,
	tmplData *TemplateData,
	fileSet *config.ResolvedFileSet,
	manifestRefs []bundleFileRef,
	hookRefs []hookRef,
) {
	allPaths := slices.Concat(
		fileSet.Networks, fileSet.Systemd, fileSet.Volumes,
		fileSet.Containers, fileSet.Kube,
	)
	for _, ref := range manifestRefs {
		allPaths = append(allPaths, ref.SrcPath)
	}
	for _, ref := range hookRefs {
		allPaths = append(allPaths, ref.SrcPath)
	}
	// Include secret entries that are templates — they may call provider
	// reader functions. Direct provider refs (op://, pass://) are not
	// templates and are skipped.
	for _, path := range fileSet.Secrets {
		if !op.IsRef(path) && !pp.IsRef(path) {
			allPaths = append(allPaths, path)
		}
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
		Category: "secret",
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
