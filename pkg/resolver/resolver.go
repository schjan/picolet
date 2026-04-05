package resolver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
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

	registry, err := BuildRegistry(ctx, r.fsys, r.secretReader, r.opSecretReader)
	if err != nil {
		return nil, fmt.Errorf("building template registry: %w", err)
	}

	fileSet := r.cfg.Assignments.Resolve(host)
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
			content, ok := opResolved[path]
			if !ok {
				if r.opSecretReader != nil {
					slog.Warn("skipping failed op:// secret", "ref", path)
				}
				continue
			}
			f, err := r.buildOpSecretFile(path, content)
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
// Failed refs are logged and omitted — successful refs still deploy (partial failure resilience).
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
	// Validate all refs before making the API call.
	for _, ref := range opRefs {
		if _, err := op.ParseOpRef(ref); err != nil {
			return nil, err
		}
	}
	slog.Debug("batch-resolving 1password secrets", "count", len(opRefs))
	results, err := r.opSecretReader(ctx, opRefs)
	if err != nil {
		slog.Warn("some 1password secrets failed to resolve", "error", err)
	}
	return results, nil
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
	return "<secret>", nil
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
