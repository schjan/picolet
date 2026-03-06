package resolver

import (
	"bytes"
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
	FS           fs.FS
	Config       *config.Config
	SecretReader SecretReader
	Rootless     bool
}

// Resolver renders templates and resolves the desired state for hosts.
type Resolver struct {
	fsys         fs.FS
	cfg          *config.Config
	secretReader SecretReader
	quadletDir   string
	systemdDir   string
	dataDir      string
}

// New creates a new Resolver.
// Pass nil for SecretReader to use placeholder mode (validate/CI).
// When Rootless is true, destination paths use ~/.config/ and ~/.local/share/ instead of /etc/ and /var/lib/.
func New(rc Config) (*Resolver, error) {
	quadletDir, systemdDir, dataDir, err := resolveDirs(rc.Rootless)
	if err != nil {
		return nil, err
	}
	return &Resolver{
		fsys:         rc.FS,
		cfg:          rc.Config,
		secretReader: rc.SecretReader,
		quadletDir:   quadletDir,
		systemdDir:   systemdDir,
		dataDir:      dataDir,
	}, nil
}

// resolveDirs computes destination directories based on rootless mode.
func resolveDirs(rootless bool) (quadletDir, systemdDir, dataDir string, err error) {
	if !rootless {
		return "/etc/containers/systemd", "/etc/systemd/system", "/var/lib/picolet", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", fmt.Errorf("getting home directory: %w", err)
	}
	return filepath.Join(home, ".config", "containers", "systemd"),
		filepath.Join(home, ".config", "systemd", "user"),
		filepath.Join(home, ".local", "share", "picolet"), nil
}

// ResolveHost computes the complete desired state for a given hostname.
//
//nolint:cyclop // sequential resolution of file categories; splitting would obscure the flow
func (r *Resolver) ResolveHost(hostname string) (*ResolvedHost, error) {
	host, ok := r.cfg.Hosts[hostname]
	if !ok {
		return nil, &HostNotFoundError{Hostname: hostname}
	}

	tmplData, err := NewTemplateData(r.cfg, hostname)
	if err != nil {
		return nil, err
	}

	registry, err := BuildRegistry(r.fsys, r.secretReader)
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

	for _, path := range fileSet.Secrets {
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
func (r *Resolver) ResolveAll() (map[string]*ResolvedHost, error) {
	results := make(map[string]*ResolvedHost, len(r.cfg.Hosts))
	for _, hostname := range r.cfg.SortedHostnames() {
		resolved, err := r.ResolveHost(hostname)
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

// secretContent returns the content for a secret entry.
// Template secrets are rendered with the full template engine.
// Static secrets are read from SecretsDir via secretReader (never from the repo).
func (r *Resolver) secretContent(registry *template.Template, tmplData *TemplateData, srcPath, filename string) (string, error) {
	if strings.HasSuffix(srcPath, ".tmpl") {
		return r.renderOrRead(registry, tmplData, srcPath)
	}
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
