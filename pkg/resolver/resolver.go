package resolver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/containers/podman/v5/pkg/systemd/quadlet"

	"github.com/schjan/picolet/pkg/config"
	op "github.com/schjan/picolet/pkg/onepassword"
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
}

// ResolvedHost is the complete desired state for a single host.
type ResolvedHost struct {
	Hostname string
	Files    []ResolvedFile
}

// Config holds configuration for creating a Resolver.
type Config struct {
	FS             fs.FS
	Config         *config.Config
	SecretReader   SecretReader
	OpSecretReader OpSecretReader
	Rootless       bool
}

// Resolver renders templates and resolves the desired state for hosts.
type Resolver struct {
	fsys           fs.FS
	cfg            *config.Config
	secretReader   SecretReader
	opSecretReader OpSecretReader
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
	return &Resolver{
		fsys:           rc.FS,
		cfg:            rc.Config,
		secretReader:   rc.SecretReader,
		opSecretReader: rc.OpSecretReader,
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

	registry, opCache, err := BuildRegistry(ctx, r.fsys, r.secretReader, r.opSecretReader)
	if err != nil {
		return nil, fmt.Errorf("building template registry: %w", err)
	}

	// Fail fast on destination collisions before paying for template rendering
	// or 1Password SDK calls. DestPath is knowable from the file layout alone.
	fileSet, manifestRefs, err := r.expandAndValidate(r.cfg.Assignments.Resolve(host))
	if err != nil {
		return nil, err
	}

	// Two-phase op:// secret resolution for templates:
	// Phase 1 (collect): render all templates to discover readOpSecret calls (output discarded).
	// Phase 2 (resolve): batch-resolve collected refs, then render templates for real.
	if opCache != nil {
		r.collectOpTemplateRefs(registry, tmplData, fileSet, manifestRefs)
		if err := opCache.Resolve(ctx); err != nil {
			return nil, err
		}
	}

	// Batch-resolve op:// secrets in a single SDK call.
	opResolved, err := r.batchResolveOpSecrets(ctx, fileSet.Secrets)
	if err != nil {
		return nil, err
	}

	files, err := r.buildFiles(registry, tmplData, fileSet, manifestRefs, opResolved)
	if err != nil {
		return nil, err
	}

	return &ResolvedHost{
		Hostname: hostname,
		Files:    files,
	}, nil
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

// expandAndValidate expands service bundles into the file set and fails fast
// if any two sources resolve to the same destination path.
func (r *Resolver) expandAndValidate(fileSet *config.ResolvedFileSet) (*config.ResolvedFileSet, []manifestRef, error) {
	merged, manifestRefs, err := r.expandFileSet(fileSet)
	if err != nil {
		return nil, nil, err
	}
	skeletons, err := r.buildFileSkeletons(merged, manifestRefs)
	if err != nil {
		return nil, nil, err
	}
	if err := detectCollisions(skeletons); err != nil {
		return nil, nil, err
	}
	return merged, manifestRefs, nil
}

// expandFileSet returns a new ResolvedFileSet merged with any service bundles,
// plus the full list of manifest refs (legacy + bundled). The input fileSet is
// not mutated. Manifests and Services are left nil: manifest paths flow through
// manifestRefs, and Services is already flattened into the category slices.
// Populating them would let a future caller miss bundle contents.
func (r *Resolver) expandFileSet(fileSet *config.ResolvedFileSet) (*config.ResolvedFileSet, []manifestRef, error) {
	expanded, err := expandServiceBundles(r.fsys, fileSet.Services)
	if err != nil {
		return nil, nil, err
	}

	merged := &config.ResolvedFileSet{
		Networks:   sortedUnique(slices.Concat(fileSet.Networks, expanded.Networks)),
		Systemd:    sortedUnique(slices.Concat(fileSet.Systemd, expanded.Systemd)),
		Volumes:    sortedUnique(slices.Concat(fileSet.Volumes, expanded.Volumes)),
		Containers: sortedUnique(slices.Concat(fileSet.Containers, expanded.Containers)),
		Kube:       sortedUnique(slices.Concat(fileSet.Kube, expanded.Kube)),
		Secrets:    sortedUnique(slices.Concat(fileSet.Secrets, expanded.Secrets)),
	}

	manifestRefs := make([]manifestRef, 0, len(fileSet.Manifests)+len(expanded.Manifests))
	for _, srcPath := range fileSet.Manifests {
		manifestRefs = append(manifestRefs, newLegacyManifestRef(srcPath))
	}
	manifestRefs = append(manifestRefs, expanded.Manifests...)
	return merged, uniqueManifestRefs(manifestRefs), nil
}

// newLegacyManifestRef constructs a manifestRef for a legacy (non-bundled)
// manifest path, where the source and logical paths are the same. Bundled
// refs set a stripped LogicalPath and are built in readManifestSubdir.
func newLegacyManifestRef(srcPath string) manifestRef {
	return manifestRef{SrcPath: srcPath, LogicalPath: srcPath}
}

// buildFileSkeletons returns SrcPath/Category/DestPath tuples for every file
// the host will deploy. It does not render templates, read files, or call the
// 1Password SDK, so it's safe (and cheap) to run before expensive operations.
func (r *Resolver) buildFileSkeletons(fileSet *config.ResolvedFileSet, manifestRefs []manifestRef) ([]ResolvedFile, error) {
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
			SrcPath: ref.SrcPath, Category: "manifest", DestPath: r.manifestDestPath(ref.LogicalPath),
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

func (r *Resolver) manifestDestPath(logicalPath string) string {
	return filepath.Join(r.dataDir, filepath.FromSlash(strings.TrimSuffix(logicalPath, ".tmpl")))
}

// secretDestPath returns the DestPath for either an op:// ref or a file-based
// secret. ParseOpRef is pure — no network call.
func (r *Resolver) secretDestPath(srcPath string) (string, error) {
	if op.IsRef(srcPath) {
		parsed, err := op.ParseOpRef(srcPath)
		if err != nil {
			return "", err
		}
		return "secret:" + parsed.PodmanSecretName(), nil
	}
	filename := destFilename(srcPath)
	return "secret:" + strings.TrimSuffix(filename, filepath.Ext(filename)), nil
}

func uniqueManifestRefs(refs []manifestRef) []manifestRef {
	slices.SortFunc(refs, func(a, b manifestRef) int {
		if diff := strings.Compare(a.LogicalPath, b.LogicalPath); diff != 0 {
			return diff
		}
		return strings.Compare(a.SrcPath, b.SrcPath)
	})
	return slices.CompactFunc(refs, func(a, b manifestRef) bool {
		return a == b
	})
}

func (r *Resolver) buildFiles(
	registry *template.Template,
	tmplData *TemplateData,
	fileSet *config.ResolvedFileSet,
	manifestRefs []manifestRef,
	opResolved map[string]string,
) ([]ResolvedFile, error) {
	var files []ResolvedFile

	standardFiles, err := r.buildStandardFiles(registry, tmplData, fileSet)
	if err != nil {
		return nil, err
	}
	files = append(files, standardFiles...)

	for _, ref := range manifestRefs {
		f, err := r.resolveManifestRef(registry, tmplData, ref)
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
	opResolved map[string]string,
) ([]ResolvedFile, error) {
	var files []ResolvedFile
	for _, srcPath := range secrets {
		if op.IsRef(srcPath) {
			if opResolved == nil {
				continue
			}
			f, err := r.buildOpSecretFile(srcPath, opResolved[srcPath])
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

// destFilename returns the base filename for a source path, stripping any .tmpl suffix.
func destFilename(srcPath string) string {
	return strings.TrimSuffix(path.Base(srcPath), ".tmpl")
}

func (r *Resolver) resolveManifestRef(registry *template.Template, tmplData *TemplateData, ref manifestRef) (*ResolvedFile, error) {
	content, err := r.renderOrRead(registry, tmplData, ref.SrcPath)
	if err != nil {
		return nil, fmt.Errorf("resolving manifest %s: %w", ref.SrcPath, err)
	}

	return &ResolvedFile{
		SrcPath:  ref.SrcPath,
		DestPath: r.manifestDestPath(ref.LogicalPath),
		Content:  content,
		Category: "manifest",
	}, nil
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

// batchResolveOpSecrets collects all op:// refs from the secrets list and resolves
// them in a single SDK call. Returns a map from ref to secret value.
// Any resolution failure is fatal to prevent reconciler.Diff from marking
// unresolved secrets for deletion (which would remove them from Podman).
// When 1Password is not configured, all op:// refs are skipped with a warning.
//
//nolint:nilnil // nil map signals "no op:// secrets to resolve" or "1Password not configured"
func (r *Resolver) batchResolveOpSecrets(ctx context.Context, allSecrets []string) (map[string]string, error) {
	var opRefs []string
	for _, path := range allSecrets {
		if op.IsRef(path) {
			opRefs = append(opRefs, path)
		}
	}
	if len(opRefs) == 0 {
		return nil, nil
	}
	if r.opSecretReader == nil {
		slog.Warn("skipping op:// secrets (1password not configured)", "count", len(opRefs))
		return nil, nil
	}
	slog.Debug("batch-resolving 1password secrets", "count", len(opRefs))
	results, err := r.opSecretReader(ctx, opRefs)
	if err != nil {
		return nil, fmt.Errorf("resolving 1password secrets: %w", err)
	}
	return results, nil
}

// collectOpTemplateRefs executes all .tmpl files in collect mode to discover readOpSecret calls.
// Output is discarded — only the side effect of populating the OpSecretCache matters.
func (r *Resolver) collectOpTemplateRefs(
	registry *template.Template,
	tmplData *TemplateData,
	fileSet *config.ResolvedFileSet,
	manifestRefs []manifestRef,
) {
	allPaths := slices.Concat(
		fileSet.Networks, fileSet.Systemd, fileSet.Volumes,
		fileSet.Containers, fileSet.Kube,
	)
	for _, ref := range manifestRefs {
		allPaths = append(allPaths, ref.SrcPath)
	}
	// Include non-op:// secrets that are templates (they might call readOpSecret).
	for _, path := range fileSet.Secrets {
		if !op.IsRef(path) {
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

// buildOpSecretFile creates a ResolvedFile for a pre-resolved op:// secret.
func (r *Resolver) buildOpSecretFile(ref, content string) (*ResolvedFile, error) {
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
