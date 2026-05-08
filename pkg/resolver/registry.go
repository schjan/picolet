package resolver

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	sprig "github.com/Masterminds/sprig/v3"

	"github.com/schjan/picolet/pkg/config"
	op "github.com/schjan/picolet/pkg/onepassword"
	pp "github.com/schjan/picolet/pkg/protonpass"
)

const maxTemplateDepth = 10

// SecretReader reads secret file content.
// Pass nil for placeholder mode (validate/CI).
type SecretReader func(path string) (string, error)

// SecretRefReader resolves secret references in batch (e.g. op:// or pass://).
// Returns successfully resolved secrets and per-reference errors separately.
// Pass nil to disable a provider (its template function returns a placeholder).
type SecretRefReader func(ctx context.Context, refs []string) (map[string]string, error)

// OpSecretReader is a backward-compatible alias for SecretRefReader.
type OpSecretReader = SecretRefReader

// ProviderKey identifies a secret provider inside the template registry.
type ProviderKey string

const (
	ProviderOnePassword ProviderKey = "onepassword"
	ProviderProtonPass  ProviderKey = "protonpass"
)

// ProviderTemplate describes a secret-provider integration for the template registry.
//
// Key is used only to return the provider's cache without relying on slice order.
// FuncName is the Go template function name (e.g. "readOpSecret").
// IsRef returns true when a string is a syntactically valid reference for the provider.
// PlaceholderEmpty is returned when the provider is not configured (Reader == nil).
// PlaceholderPending is returned during the collect phase before refs are resolved.
type ProviderTemplate struct {
	Key                ProviderKey
	FuncName           string
	Reader             SecretRefReader
	IsRef              func(string) bool
	PlaceholderEmpty   string
	PlaceholderPending string
}

// OpProvider returns the standard 1Password provider template.
// reader may be nil to disable the provider.
func OpProvider(reader SecretRefReader) ProviderTemplate {
	return ProviderTemplate{
		Key:                ProviderOnePassword,
		FuncName:           "readOpSecret",
		Reader:             reader,
		IsRef:              op.IsRef,
		PlaceholderEmpty:   "<op-secret>",
		PlaceholderPending: "<op-secret-pending>",
	}
}

// PPProvider returns the standard Proton Pass provider template.
// reader may be nil to disable the provider.
func PPProvider(reader SecretRefReader) ProviderTemplate {
	return ProviderTemplate{
		Key:                ProviderProtonPass,
		FuncName:           "readProtonPassSecret",
		Reader:             reader,
		IsRef:              pp.IsRef,
		PlaceholderEmpty:   "<pp-secret>",
		PlaceholderPending: "<pp-secret-pending>",
	}
}

const placeholderSecret = "<secret>"

// RefCache manages two-phase resolution of secret references for templates.
// Avoids N individual provider round-trips when templates use multiple
// reader-function calls.
//
// Not goroutine-safe; template rendering must be serial (same constraint as renderTemplate).
type RefCache struct {
	reader    SecretRefReader
	collected []string
	resolved  map[string]string // non-nil after Resolve(); doubles as phase indicator
}

// ProviderCaches stores per-provider caches by key so callers do not depend on
// the provider slice order.
type ProviderCaches map[ProviderKey]*RefCache

// ResolveAll resolves all configured provider caches.
func (c ProviderCaches) ResolveAll(ctx context.Context) error {
	for _, key := range sortedCacheKeys(c) {
		if err := c[key].Resolve(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Resolve batch-resolves all collected refs. After this call, the cache returns
// resolved values for any subsequent template execution.
func (c *RefCache) Resolve(ctx context.Context) error {
	if len(c.collected) == 0 {
		c.resolved = make(map[string]string)
		return nil
	}
	unique := sortedUnique(c.collected)
	slog.Debug("batch-resolving template secrets", "count", len(unique))
	results, err := c.reader(ctx, unique)
	if err != nil {
		return fmt.Errorf("resolving template secrets: %w", err)
	}
	c.resolved = results
	return nil
}

// BuildRegistry collects all .tmpl files from the filesystem and builds
// a shared template registry with Sprig + picolet-specific functions.
//
// For each ProviderTemplate with a non-nil Reader, a RefCache is created and
// the corresponding template function is registered. The returned caches map
// contains entries only for providers whose Reader is non-nil.
//
// Two-phase resolution: callers must run a collect pass, call cache.Resolve,
// then run the real render pass.
//
//nolint:cyclop,funlen // funcmap registration is inherently branchy
func BuildRegistry(ctx context.Context, fsys fs.FS, secretReader SecretReader, providers []ProviderTemplate, dataDir string) (*template.Template, ProviderCaches, error) {
	sources, err := loadTemplateSources(fsys)
	if err != nil {
		return nil, nil, err
	}
	if err := validateProviderKeys(providers); err != nil {
		return nil, nil, err
	}

	caches := make(ProviderCaches)
	for _, p := range providers {
		if p.Reader != nil {
			caches[p.Key] = &RefCache{reader: p.Reader}
		}
	}

	var root *template.Template
	funcMap := sprig.HermeticTxtFuncMap()
	maps.Copy(funcMap, template.FuncMap{
		// Keep historical picolet semantics: do not indent empty lines.
		"indent":  indentFunc,
		"nindent": nindentFunc,
		"readFile": func(path string) (string, error) {
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				return "", fmt.Errorf("readFile %q: %w", path, err)
			}
			return string(data), nil
		},
		"glob": func(patterns ...string) ([]string, error) {
			return globFunc(fsys, patterns...)
		},
		"concatFiles": func(patterns ...string) (string, error) {
			return concatFilesFunc(fsys, patterns...)
		},
		"readSecretFile": func(path string) (string, error) {
			if secretReader == nil {
				return placeholderSecret, nil
			}
			return secretReader(path)
		},
		"manifestPath": func(relPath string) (string, error) {
			cleaned, err := config.ValidateRelPath(relPath)
			if err != nil {
				return "", fmt.Errorf("manifestPath %q: %w", relPath, err)
			}
			return filepath.Join(dataDir, "manifests", filepath.FromSlash(cleaned)), nil
		},
		"filePath": func(relPath string) (string, error) {
			cleaned, err := config.ValidateRelPath(relPath)
			if err != nil {
				return "", fmt.Errorf("filePath %q: %w", relPath, err)
			}
			return filepath.Join(dataDir, "files", filepath.FromSlash(cleaned)), nil
		},
		// renderTemplate uses a closure depth counter to prevent infinite recursion.
		// Not goroutine-safe; template rendering must be serial.
		"renderTemplate": func() any {
			var depth int
			return func(name string, data any) (string, error) {
				depth++
				if depth > maxTemplateDepth {
					return "", fmt.Errorf("renderTemplate %q: recursion depth exceeded (%d)", name, maxTemplateDepth)
				}
				defer func() { depth-- }()
				var buf bytes.Buffer
				if err := root.ExecuteTemplate(&buf, name, data); err != nil {
					return "", fmt.Errorf("renderTemplate %q: %w", name, err)
				}
				return buf.String(), nil
			}
		}(),
	})

	for i := range providers {
		registerProviderFunc(ctx, funcMap, providers[i], caches[providers[i].Key])
	}

	root = template.New("").Option("missingkey=error").Funcs(funcMap)
	for name, src := range sources {
		if _, err := root.New(name).Parse(src); err != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", name, err)
		}
	}
	return root, caches, nil
}

func validateProviderKeys(providers []ProviderTemplate) error {
	seen := make(map[ProviderKey]string, len(providers))
	for _, p := range providers {
		if p.Key == "" {
			return fmt.Errorf("provider %q has empty key", p.FuncName)
		}
		if prev, ok := seen[p.Key]; ok {
			return fmt.Errorf("provider key %q used by both %q and %q", p.Key, prev, p.FuncName)
		}
		seen[p.Key] = p.FuncName
	}
	return nil
}

func sortedCacheKeys(c ProviderCaches) []ProviderKey {
	keys := slices.Collect(maps.Keys(c))
	slices.Sort(keys)
	return keys
}

func loadTemplateSources(fsys fs.FS) (map[string]string, error) {
	sources := make(map[string]string)
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".tmpl") {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		sources[path] = string(data)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking templates: %w", err)
	}
	return sources, nil
}

// registerProviderFunc adds a provider's template function to funcMap.
// When the provider's Reader is nil (cache == nil), the function returns the
// configured "empty" placeholder. Otherwise it participates in the two-phase
// collect+resolve cycle.
func registerProviderFunc(ctx context.Context, funcMap template.FuncMap, p ProviderTemplate, cache *RefCache) {
	funcMap[p.FuncName] = func(ref string) (string, error) {
		if cache == nil {
			return p.PlaceholderEmpty, nil
		}
		if !p.IsRef(ref) {
			return "", fmt.Errorf("%s: %q is not a valid reference for this provider", p.FuncName, ref)
		}
		if cache.resolved == nil {
			cache.collected = append(cache.collected, ref)
			return p.PlaceholderPending, nil
		}
		if val, ok := cache.resolved[ref]; ok {
			return val, nil
		}
		// Cache miss: dynamic ref not seen in collect phase.
		slog.Debug("cache miss, resolving individually", "func", p.FuncName, "ref", ref)
		results, err := p.Reader(ctx, []string{ref})
		if err != nil {
			return "", err
		}
		val, ok := results[ref]
		if !ok {
			return "", fmt.Errorf("%s: ref %q failed to resolve", p.FuncName, ref)
		}
		return val, nil
	}
}

func indentFunc(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i := range lines {
		if lines[i] != "" {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func nindentFunc(n int, s string) string {
	return "\n" + indentFunc(n, s)
}

func globFunc(fsys fs.FS, patterns ...string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, fmt.Errorf("glob: at least one pattern is required")
	}

	var paths []string
	for _, pattern := range patterns {
		matches, err := fs.Glob(fsys, pattern)
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", pattern, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("glob %q: no files matched", pattern)
		}
		paths = append(paths, matches...)
	}

	slices.Sort(paths)
	return slices.Compact(paths), nil
}

func concatFilesFunc(fsys fs.FS, patterns ...string) (string, error) {
	paths, err := globFunc(fsys, patterns...)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	var prev string
	for _, path := range paths {
		data, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return "", fmt.Errorf("concatFiles %q: %w", path, readErr)
		}
		current := string(data)
		if needsNewlineSeparator(prev, current) {
			b.WriteByte('\n')
		}
		b.WriteString(current)
		prev = current
	}

	return b.String(), nil
}

func needsNewlineSeparator(left, right string) bool {
	return left != "" &&
		right != "" &&
		!strings.HasSuffix(left, "\n") &&
		!strings.HasPrefix(right, "\n")
}
