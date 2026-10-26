package resolver

import (
	"bytes"
	"context"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secretItem is a value a provider returns that a template then builds a
// ref from.
const secretItem = "s3cret-item"

// A ref a template builds at render time (here from template data standing
// in for a secret value) never reaches a resolver error: an unresolved dynamic ref names the function only, an
// invalid one too, and a strict-mode or no-reader error names the ref's
// scheme and file or item part, never a sops:// pointer or a field.
func TestResolverErrorsKeepDynamicRefsOut(t *testing.T) {
	t.Parallel()
	reader := func(_ context.Context, refs []string) (map[string]string, error) {
		values := map[string]string{}
		for _, ref := range refs {
			if ref == "op://vault/item/user" {
				values[ref] = secretItem
			}
		}
		return values, nil
	}
	render := func(t *testing.T, p ProviderTemplate, body string) error {
		t.Helper()
		fsys := fstest.MapFS{"s.tmpl": &fstest.MapFile{Data: []byte(body)}}
		registry, caches, err := BuildRegistry(t.Context(), fsys, nil, []ProviderTemplate{p}, "/var/lib/picolet")
		require.NoError(t, err)
		var discard bytes.Buffer
		data := map[string]string{"item": secretItem}
		if err := registry.ExecuteTemplate(&discard, "s.tmpl", data); err != nil {
			return err
		}
		if cache := caches[p.Key]; cache != nil {
			require.NoError(t, cache.Resolve(t.Context()))
		}
		return registry.ExecuteTemplate(&discard, "s.tmpl", data)
	}

	t.Run("unresolved dynamic ref", func(t *testing.T) {
		t.Parallel()
		err := render(t, OpProvider(reader), `{{ readOpSecret (printf "op://vault/%s/pw" (readOpSecret "op://vault/item/user")) }}`)
		require.ErrorContains(t, err, "readOpSecret")
		assert.NotContains(t, err.Error(), secretItem)
	})
	t.Run("invalid dynamic ref", func(t *testing.T) {
		t.Parallel()
		err := render(t, OpProvider(reader), `{{ readOpSecret .item }}`)
		require.ErrorContains(t, err, "not a valid reference")
		assert.NotContains(t, err.Error(), secretItem)
	})
	t.Run("strict mode", func(t *testing.T) {
		t.Parallel()
		p := OpProvider(reader)
		p.Strict = true
		err := render(t, p, `{{ readOpSecret (printf "op://vault/item/%s" .item) }}`)
		require.ErrorContains(t, err, `readOpSecret "op://vault/item/…": provider not configured in strict mode`)
		assert.NotContains(t, err.Error(), secretItem)
	})
	t.Run("no SOPS reader", func(t *testing.T) {
		t.Parallel()
		err := render(t, SopsProvider(nil, ""), `{{ readSopsSecret (printf "sops://secrets/db.sops.yaml#/tokens/%s" .item) }}`)
		require.ErrorContains(t, err, `readSopsSecret "sops://secrets/db.sops.yaml#…"`)
		assert.NotContains(t, err.Error(), secretItem)
	})
}

// refForError keeps a ref's scheme and its file or item part.
func TestRefForError(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string]string{ //nolint:gosec // G101: refs, no credentials
		"sops://secrets/db.sops.yaml#/db/password": "sops://secrets/db.sops.yaml#…",
		"sops://secrets/db.sops.yaml":              "sops://secrets/db.sops.yaml",
		"op://vault/item/field":                    "op://vault/item/…",
		"pass://share/item/field":                  "pass://share/item/…",
		"op://vault":                               "op://…",
	} {
		assert.Equal(t, want, refForError(ref), ref)
	}
}
