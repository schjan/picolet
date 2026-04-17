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
//
//nolint:cyclop,funlen // sequential resolution of file categories; splitting would obscure the flow
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

	fileSet := r.cfg.Assignments.Resolve(host)
	manifestRefs, err := r.expandFileSet(fileSet)
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
	if err := detectCollisions(files); err != nil {
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

func (r *Resolver) expandFileSet(fileSet *config.ResolvedFileSet) ([]manifestRef, error) {
	expanded, err := expandServiceBundles(r.fsys, fileSet.Services)
	if err != nil {
		return nil, err
	}

	fileSet.Networks = sortedUniqueStrings(append(fileSet.Networks, expanded.Networks...))
	fileSet.Systemd = sortedUniqueStrings(append(fileSet.Systemd, expanded.Systemd...))
	fileSet.Volumes = sortedUniqueStrings(append(fileSet.Volumes, expanded.Volumes...))
	fileSet.Containers = sortedUniqueStrings(append(fileSet.Containers, expanded.Containers...))
	fileSet.Kube = sortedUniqueStrings(append(fileSet.Kube, expanded.Kube...))
	fileSet.Secrets = sortedUniqueStrings(append(fileSet.Secrets, expanded.Secrets...))

	manifestRefs := make([]manifestRef, 0, len(fileSet.Manifests)+len(expanded.Manifests))
	for _, srcPath := range fileSet.Manifests {
		manifestRefs = append(manifestRefs, manifestRef{SrcPath: srcPath, LogicalPath: srcPath})
	}
	manifestRefs = append(manifestRefs, expanded.Manifests...)
	return uniqueManifestRefs(manifestRefs), nil
}

func uniqueManifestRefs(refs []manifestRef) []manifestRef {
	seen := make(map[string]struct{}, len(refs))
	unique := make([]manifestRef, 0, len(refs))
	for _, ref := range refs {
		key := ref.SrcPath + "\x00" + ref.LogicalPath
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, ref)
	}
	slices.SortFunc(unique, func(a, b manifestRef) int {
		if diff := strings.Compare(a.LogicalPath, b.LogicalPath); diff != 0 {
			return diff
		}
		return strings.Compare(a.SrcPath, b.SrcPath)
	})
	return unique
}

func (r *Resolver) buildFiles(
	registry *template.Template,
	tmplData *TemplateData,
	fileSet *config.ResolvedFileSet,
	manifestRefs []manifestRef,
	opResolved map[string]string,
) ([]ResolvedFile, error) {
	var files []ResolvedFile

	fileGroups := []struct {
		paths   []string
		cat     string
		destDir string
		quadlet bool
	}{
		{fileSet.Networks, "network", r.quadletDir, true},
		{fileSet.Systemd, "systemd", r.systemdDir, false},
		{fileSet.Volumes, "volume", r.quadletDir, true},
		{fileSet.Containers, "container", r.quadletDir, true},
		{fileSet.Kube, "kube", r.quadletDir, true},
	}
	for _, g := range fileGroups {
		for _, srcPath := range g.paths {
			f, err := r.resolveFile(registry, tmplData, srcPath, g.cat, g.destDir, g.quadlet)
			if err != nil {
				return nil, err
			}
			files = append(files, *f)
		}
	}

	for _, ref := range manifestRefs {
		f, err := r.resolveManifestRef(registry, tmplData, ref)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}

	for _, srcPath := range fileSet.Secrets {
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

	var errs []error
	for destPath, srcPaths := range collisions {
		uniquePaths := sortedUniqueStrings(srcPaths)
		if len(uniquePaths) < 2 {
			continue
		}
		errs = append(errs, fmt.Errorf("destination collision for %s: %s", destPath, strings.Join(uniquePaths, ", ")))
	}
	return errors.Join(errs...)
}

func (r *Resolver) resolveFile(registry *template.Template, tmplData *TemplateData, srcPath, category, destDir string, quadlet bool) (*ResolvedFile, error) {
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
		DestPath:    filepath.Join(destDir, filename),
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

	// manifests/<app>/deployment.yml.tmpl → <dataDir>/manifests/<app>/deployment.yml
	relPath := strings.TrimSuffix(ref.LogicalPath, ".tmpl")
	destPath := filepath.Join(r.dataDir, filepath.FromSlash(relPath))

	return &ResolvedFile{
		SrcPath:  ref.SrcPath,
		DestPath: destPath,
		Content:  content,
		Category: "manifest",
	}, nil
}

func (r *Resolver) resolveSecret(registry *template.Template, tmplData *TemplateData, srcPath string) (*ResolvedFile, error) {
	// secrets/prometheus_config.yml.tmpl → secret name "prometheus_config"
	filename := destFilename(srcPath)
	secretName := strings.TrimSuffix(filename, filepath.Ext(filename))

	content, err := r.secretContent(registry, tmplData, srcPath, filename)
	if err != nil {
		return nil, fmt.Errorf("resolving secret %s: %w", srcPath, err)
	}

	return &ResolvedFile{
		SrcPath:  srcPath,
		DestPath: "secret:" + secretName,
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
	parsed, err := op.ParseOpRef(ref)
	if err != nil {
		return nil, err
	}
	return &ResolvedFile{
		SrcPath:  ref,
		DestPath: "secret:" + parsed.PodmanSecretName(),
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
