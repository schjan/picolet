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
)

const maxTemplateDepth = 10

// SecretReader reads secret file content.
// Pass nil for placeholder mode (validate/CI).
type SecretReader func(path string) (string, error)

// OpSecretReader resolves a 1Password secret reference (e.g. "op://vault/item/field").
// Pass nil to disable 1Password integration (readOpSecret returns a placeholder).
type OpSecretReader func(ctx context.Context, ref string) (string, error)

// BuildRegistry collects all .tmpl files from the filesystem and builds
// a shared template registry with custom functions.
//
//nolint:cyclop,funlen // funcmap registration is inherently branchy
func BuildRegistry(ctx context.Context, fsys fs.FS, secretReader SecretReader, opSecretReader OpSecretReader) (*template.Template, error) {
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
		return nil, fmt.Errorf("walking templates: %w", err)
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
				return "<secret>", nil
			}
			return secretReader(path)
		},
		"readOpSecret": func(ref string) (string, error) {
			if opSecretReader == nil {
				return "<op-secret>", nil
			}
			slog.Debug("resolving 1password secret", "ref", ref)
			return opSecretReader(ctx, ref)
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
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
	}
	return root, nil
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
