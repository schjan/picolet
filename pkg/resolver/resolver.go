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

	// Two-phase op:// secret resolution for templates:
	// Phase 1 (collect): render all templates to discover readOpSecret calls (output discarded).
	// Phase 2 (resolve): batch-resolve collected refs, then render templates for real.
	if opCache != nil {
		r.collectOpTemplateRefs(registry, tmplData, fileSet)
		if err := opCache.Resolve(ctx); err != nil {
			return nil, err
		}
	}

	var files []ResolvedFile

	// Standard file categories with their destination directories.
	// quadlet=true causes the file to be parsed as a quadlet unit.
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
		for _, path := range g.paths {
			f, err := r.resolveFile(registry, tmplData, path, g.cat, g.destDir, g.quadlet)
			if err != nil {
				return nil, err
			}
			files = append(files, *f)
		}
	}

	for _, path := range fileSet.Manifests {
		f, err := r.resolveManifest(registry, tmplData, path)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}

	// Batch-resolve op:// secrets in a single SDK call.
	opResolved, err := r.batchResolveOpSecrets(ctx, fileSet.Secrets)
	if err != nil {
		return nil, err
	}

	for _, path := range fileSet.Secrets {
		if op.IsRef(path) {
			// batchResolveOpSecrets guarantees all refs are resolved or returns an error.
			// When opSecretReader is nil, opResolved is nil and IsRef entries are skipped.
			if opResolved == nil {
				continue
			}
			f, err := r.buildOpSecretFile(path, opResolved[path])
			if err != nil {
				return nil, err
			}
			files = append(files, *f)
			continue
		}
		f, err := r.resolveSecret(registry, tmplData, path)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}

	// Group aggregate secrets by name so that multiple layers can contribute
	// different globs to the same Podman secret (additive merge).
	// Deduplication of identical (name, glob) pairs has already happened in config.Resolve.
	for _, ag := range groupAggregateSecrets(fileSet.AggregateSecrets) {
		f, err := r.resolveAggregateSecret(ag.Name, ag.Header, ag.Globs)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}

	// Guard against a secret name being defined as both a regular secret and an
	// aggregate secret — the reconciler keys on DestPath, so duplicates would
	// cause non-deterministic overwrites.
	if err := checkSecretNameCollisions(files); err != nil {
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
	return strings.TrimSuffix(filepath.Base(srcPath), ".tmpl")
}

func (r *Resolver) resolveManifest(registry *template.Template, tmplData *TemplateData, srcPath string) (*ResolvedFile, error) {
	content, err := r.renderOrRead(registry, tmplData, srcPath)
	if err != nil {
		return nil, fmt.Errorf("resolving manifest %s: %w", srcPath, err)
	}

	// manifests/<app>/deployment.yml.tmpl → <dataDir>/manifests/<app>/deployment.yml
	relPath := strings.TrimSuffix(srcPath, ".tmpl")
	destPath := filepath.Join(r.dataDir, relPath)

	return &ResolvedFile{
		SrcPath:  srcPath,
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
func (r *Resolver) collectOpTemplateRefs(registry *template.Template, tmplData *TemplateData, fileSet *config.ResolvedFileSet) {
	allPaths := slices.Concat(
		fileSet.Networks, fileSet.Systemd, fileSet.Volumes,
		fileSet.Containers, fileSet.Kube, fileSet.Manifests,
	)
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

// checkSecretNameCollisions returns an error if any secret DestPath appears more
// than once — which would happen when a regular secret and an aggregate secret
// resolve to the same Podman secret name.
func checkSecretNameCollisions(files []ResolvedFile) error {
	seen := make(map[string]bool)
	for _, f := range files {
		if f.Category != "secret" {
			continue
		}
		if seen[f.DestPath] {
			name := strings.TrimPrefix(f.DestPath, "secret:")
			return fmt.Errorf("secret %q is defined both as a regular secret and an aggregate secret", name)
		}
		seen[f.DestPath] = true
	}
	return nil
}

// groupedAggregate holds the merged globs for a single aggregate secret name.
type groupedAggregate struct {
	Name   string
	Header string
	Globs  []string
}

// groupAggregateSecrets groups entries by name in first-seen order, collecting all globs.
// The first non-empty header encountered for a name is used.
func groupAggregateSecrets(entries []config.AggregateSecret) []groupedAggregate {
	seen := make(map[string]int) // name → index in result
	var result []groupedAggregate
	for _, ag := range entries {
		if i, ok := seen[ag.Name]; ok {
			result[i].Globs = append(result[i].Globs, ag.Glob)
			if result[i].Header == "" && ag.Header != "" {
				result[i].Header = ag.Header
			}
		} else {
			seen[ag.Name] = len(result)
			result = append(result, groupedAggregate{
				Name:   ag.Name,
				Header: ag.Header,
				Globs:  []string{ag.Glob},
			})
		}
	}
	return result
}

// resolveAggregateSecret globs files from the repo FS across all provided patterns and
// concatenates them into a single secret. Files are read verbatim (no template rendering),
// so Prometheus {{ }} expressions pass through. Returns an error if any glob matches no files.
func (r *Resolver) resolveAggregateSecret(name, header string, globs []string) (*ResolvedFile, error) {
	var allMatches []string
	for _, pattern := range globs {
		matches, err := fs.Glob(r.fsys, pattern)
		if err != nil {
			return nil, fmt.Errorf("resolving aggregate secret %q: invalid glob %q: %w", name, pattern, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("resolving aggregate secret %q: glob %q matched no files", name, pattern)
		}
		allMatches = append(allMatches, matches...)
	}
	slices.Sort(allMatches)
	allMatches = slices.Compact(allMatches) // overlapping globs may match the same file

	var buf bytes.Buffer
	buf.WriteString(header)
	for _, path := range allMatches {
		data, err := fs.ReadFile(r.fsys, path)
		if err != nil {
			return nil, fmt.Errorf("resolving aggregate secret %q: reading %s: %w", name, path, err)
		}
		// Ensure a newline separator between header/files so content is never glued together.
		if buf.Len() > 0 && buf.Bytes()[buf.Len()-1] != '\n' {
			buf.WriteByte('\n')
		}
		buf.Write(data)
	}

	return &ResolvedFile{
		SrcPath:  "aggregate:" + name,
		DestPath: "secret:" + name,
		Content:  buf.String(),
		Category: "secret",
	}, nil
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
