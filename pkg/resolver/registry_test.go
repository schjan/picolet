package resolver

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func renderRegistryTemplate(tb testing.TB, fsys fstest.MapFS, name string, data any) (string, error) {
	tb.Helper()

	registry, _, err := BuildRegistry(tb.Context(), fsys, nil, nil)
	require.NoError(tb, err)

	var buf bytes.Buffer
	err = registry.ExecuteTemplate(&buf, name, data)
	return buf.String(), err
}

func TestGlobLexicalOrderAndDedup(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"main.tmpl":         &fstest.MapFile{Data: []byte(`{{- range $i, $p := glob "fragments/*.yml" "fragments/1*.yml" -}}{{if $i}},{{end}}{{ $p }}{{- end -}}`)},
		"fragments/1.yml":   &fstest.MapFile{Data: []byte("one")},
		"fragments/2.yml":   &fstest.MapFile{Data: []byte("two")},
		"fragments/10.yml":  &fstest.MapFile{Data: []byte("ten")},
		"fragments/skip.md": &fstest.MapFile{Data: []byte("skip")},
	}

	out, err := renderRegistryTemplate(t, fsys, "main.tmpl", nil)
	require.NoError(t, err)
	assert.Equal(t, "fragments/1.yml,fragments/10.yml,fragments/2.yml", out)
}

func TestGlobInvalidPatternError(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"main.tmpl": &fstest.MapFile{Data: []byte(`{{glob "["}}`)},
	}

	_, err := renderRegistryTemplate(t, fsys, "main.tmpl", nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, `glob "["`)
}

func TestGlobEmptyMatchError(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"main.tmpl": &fstest.MapFile{Data: []byte(`{{glob "fragments/*.yml"}}`)},
	}

	_, err := renderRegistryTemplate(t, fsys, "main.tmpl", nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, `glob "fragments/*.yml": no files matched`)
}

func TestConcatFilesRawTemplatePassThrough(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"main.tmpl":           &fstest.MapFile{Data: []byte(`{{concatFiles "fragments/*.tmpl"}}`)},
		"fragments/part.tmpl": &fstest.MapFile{Data: []byte("value={{ .Host.Hostname }}")},
	}

	out, err := renderRegistryTemplate(t, fsys, "main.tmpl", map[string]any{
		"Host": map[string]any{"Hostname": "node-1"},
	})
	require.NoError(t, err)
	assert.Equal(t, "value={{ .Host.Hostname }}", out)
}

func TestConcatFilesNewlineSeparatorInjection(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"main.tmpl":          &fstest.MapFile{Data: []byte(`{{concatFiles "fragments/*.txt"}}`)},
		"fragments/01.txt":   &fstest.MapFile{Data: []byte("alpha")},
		"fragments/02.txt":   &fstest.MapFile{Data: []byte("beta\n")},
		"fragments/03.txt":   &fstest.MapFile{Data: []byte("\ngamma")},
		"fragments/04.txt":   &fstest.MapFile{Data: []byte("delta")},
		"fragments/skip.yml": &fstest.MapFile{Data: []byte("skip")},
	}

	out, err := renderRegistryTemplate(t, fsys, "main.tmpl", nil)
	require.NoError(t, err)
	assert.Equal(t, "alpha\nbeta\n\ngamma\ndelta", out)
	assert.False(t, strings.HasSuffix(out, "\n"))
}

func TestConcatFilesNoPatternsError(t *testing.T) {
	t.Parallel()

	_, err := concatFilesFunc(fstest.MapFS{}, []string{}...)
	require.Error(t, err)
	assert.ErrorContains(t, err, "at least one pattern is required")
}

func TestConcatFilesEmptyMatchError(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"fragments/one.yml": &fstest.MapFile{Data: []byte("one")},
	}

	_, err := concatFilesFunc(fsys, "fragments/*.txt")
	require.Error(t, err)
	assert.ErrorContains(t, err, `glob "fragments/*.txt": no files matched`)
}

func TestNindentSemantics(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"main.tmpl": &fstest.MapFile{Data: []byte(`{{nindent 2 "foo\nbar"}}|{{nindent 4 ""}}`)},
	}

	out, err := renderRegistryTemplate(t, fsys, "main.tmpl", nil)
	require.NoError(t, err)
	assert.Equal(t, "\n  foo\n  bar|\n    ", out)
}

func TestHasSemantics(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"main.tmpl": &fstest.MapFile{Data: []byte(`{{if has "gpu" .Host.Features}}enabled{{else}}disabled{{end}}`)},
	}

	out, err := renderRegistryTemplate(t, fsys, "main.tmpl", map[string]any{
		"Host": map[string]any{"Features": []string{"mqtt", "gpu"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "enabled", out)
}

func TestGlobAndRenderTemplateWorkflow(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"main.tmpl":            &fstest.MapFile{Data: []byte(`{{- range glob "fragments/*.tmpl" -}}{{ renderTemplate . $ }}{{- end -}}`)},
		"fragments/10.tmpl":    &fstest.MapFile{Data: []byte("first={{ .Host.Hostname }}\n")},
		"fragments/20.tmpl":    &fstest.MapFile{Data: []byte(`second={{ index .Images "app" }}`)},
		"fragments/static.yml": &fstest.MapFile{Data: []byte("ignored")},
	}

	data := map[string]any{
		"Host":   map[string]any{"Hostname": "node-1"},
		"Images": map[string]string{"app": "app:v1"},
	}

	out, err := renderRegistryTemplate(t, fsys, "main.tmpl", data)
	require.NoError(t, err)
	assert.Equal(t, "first=node-1\nsecond=app:v1", out)
}
