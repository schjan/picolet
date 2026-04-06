package resolver

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"text/template"

	op "github.com/schjan/picolet/pkg/onepassword"
)

const maxTemplateDepth = 10

// SecretReader reads secret file content.
// Pass nil for placeholder mode (validate/CI).
type SecretReader func(path string) (string, error)

// OpSecretReader resolves 1Password secret references in batch.
// Returns successfully resolved secrets and per-reference errors separately.
// Pass nil to disable 1Password integration (readOpSecret returns a placeholder).
type OpSecretReader func(ctx context.Context, refs []string) (map[string]string, error)

const (
	placeholderSecret          = "<secret>"
	placeholderOpSecret        = "<op-secret>"
	placeholderOpSecretPending = "<op-secret-pending>"
)

// OpSecretCache manages two-phase op:// secret resolution for templates.
// Avoids N individual SDK round-trips when templates use multiple readOpSecret calls.
// Not goroutine-safe; template rendering must be serial (same constraint as renderTemplate).
type OpSecretCache struct {
	reader    OpSecretReader
	collected []string
	resolved  map[string]string // non-nil after Resolve(); doubles as phase indicator
}

// Resolve batch-resolves all collected refs. After this call, readOpSecret returns cached values.
func (c *OpSecretCache) Resolve(ctx context.Context) error {
	if len(c.collected) == 0 {
		c.resolved = make(map[string]string)
		return nil
	}
	unique := slices.Compact(slices.Sorted(slices.Values(c.collected)))
	slog.Debug("batch-resolving template op:// secrets", "count", len(unique))
	results, err := c.reader(ctx, unique)
	if err != nil {
		return fmt.Errorf("resolving template 1password secrets: %w", err)
	}
	c.resolved = results
	return nil
}

// BuildRegistry collects all .tmpl files from the filesystem and builds
// a shared template registry with custom functions.
//
// When opSecretReader is non-nil, the returned OpSecretCache manages two-phase
// resolution: callers must run a collect pass, call cache.Resolve, then run
// the real render pass. When opSecretReader is nil, the cache is nil.
//
//nolint:cyclop,funlen // funcmap registration is inherently branchy
func BuildRegistry(ctx context.Context, fsys fs.FS, secretReader SecretReader, opSecretReader OpSecretReader) (*template.Template, *OpSecretCache, error) {
	sources := make(map[string]string)
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
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
		return nil, nil, fmt.Errorf("walking templates: %w", err)
	}

	var cache *OpSecretCache
	if opSecretReader != nil {
		cache = &OpSecretCache{reader: opSecretReader}
	}

	var root *template.Template
	funcMap := template.FuncMap{
		"readFile": func(path string) (string, error) {
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				return "", fmt.Errorf("readFile %q: %w", path, err)
			}
			return string(data), nil
		},
		"indent": indentFunc,
		"readSecretFile": func(path string) (string, error) {
			if secretReader == nil {
				return placeholderSecret, nil
			}
			return secretReader(path)
		},
		"readOpSecret": func(ref string) (string, error) {
			if cache == nil {
				return placeholderOpSecret, nil
			}
			if !op.IsRef(ref) {
				return "", fmt.Errorf("readOpSecret: %q is not a valid op:// reference", ref)
			}
			if cache.resolved == nil {
				cache.collected = append(cache.collected, ref)
				return placeholderOpSecretPending, nil
			}
			if val, ok := cache.resolved[ref]; ok {
				return val, nil
			}
			// Cache miss: dynamic ref not seen in collect phase.
			slog.Debug("readOpSecret: cache miss, resolving individually", "ref", ref)
			results, err := opSecretReader(ctx, []string{ref})
			if err != nil {
				return "", err
			}
			val, ok := results[ref]
			if !ok {
				return "", fmt.Errorf("readOpSecret: ref %q failed to resolve", ref)
			}
			return val, nil
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
		"has": func(item string, list []string) bool {
			return slices.Contains(list, item)
		},
	}

	root = template.New("").Option("missingkey=error").Funcs(funcMap)
	for name, src := range sources {
		if _, err := root.New(name).Parse(src); err != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", name, err)
		}
	}
	return root, cache, nil
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
