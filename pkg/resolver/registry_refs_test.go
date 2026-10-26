package resolver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secretItem is a value a provider returns that a template then builds a
// ref from.
const secretItem = "s3cret-item"

// userReader resolves op://vault/item/user to secretItem, and nothing else.
func userReader(_ context.Context, refs []string) (map[string]string, error) {
	values := map[string]string{}
	for _, ref := range refs {
		if ref == "op://vault/item/user" {
			values[ref] = secretItem
		}
	}
	return values, nil
}

// failingReader fails every ref holding secretItem, quoting it as the
// provider clients do, and resolves the rest to secretItem.
func failingReader(_ context.Context, refs []string) (map[string]string, error) {
	values := map[string]string{}
	for _, ref := range refs {
		if strings.Contains(ref, secretItem) {
			return nil, fmt.Errorf("resolving 1password secret %q: not found", ref)
		}
		values[ref] = secretItem
	}
	return values, nil
}

// failingSopsReader is failingReader as a SopsReader, quoting the ref as
// the SOPS readers do.
type failingSopsReader struct{}

func (failingSopsReader) ReadAll(_ context.Context, refs []string) (map[string]string, error) {
	values := map[string]string{}
	for _, ref := range refs {
		if strings.Contains(ref, secretItem) {
			return nil, fmt.Errorf("decrypting %q: not found", ref)
		}
		values[ref] = secretItem
	}
	return values, nil
}

func (failingSopsReader) ReadData(context.Context, string) (any, error) {
	return nil, errors.New("not used")
}

// renderTwoPhase renders body with p as the Agent does (collect pass,
// Resolve, render pass), with template data .item set to secretItem, and
// returns the first error.
func renderTwoPhase(t *testing.T, p ProviderTemplate, body string) error {
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
		if err := cache.Resolve(t.Context()); err != nil {
			return err
		}
	}
	return registry.ExecuteTemplate(&discard, "s.tmpl", data)
}

// A ref a template builds at render time (here from template data standing
// in for a secret value) never reaches a resolver error: an unresolved
// dynamic ref names the function only, an invalid one too, a strict-mode or
// no-reader error names the ref's scheme and its file or vault, never a
// sops:// pointer or an op:// item, and a provider reader's error quoting an
// op:// ref, in the batch or for a dynamic ref, quotes it the same way, as
// does a SOPS reader's error quoting a sops:// ref built at render time.
func TestResolverErrorsKeepDynamicRefsOut(t *testing.T) {
	t.Parallel()
	const fromUser = `{{ readOpSecret (printf "op://vault/%s/pw" (readOpSecret "op://vault/item/user")) }}`
	const fromData = `{{ readOpSecret (printf "op://vault/%s/pw" .item) }}`
	strict := OpProvider(userReader)
	strict.Strict = true
	for _, tc := range []struct {
		name     string
		provider ProviderTemplate
		body     string
		want     string
	}{
		{name: "unresolved dynamic ref", provider: OpProvider(userReader), body: fromUser, want: "readOpSecret"},
		{name: "invalid dynamic ref", provider: OpProvider(userReader), body: `{{ readOpSecret .item }}`, want: "not a valid reference"},
		{name: "strict mode", provider: strict, body: fromData, want: `readOpSecret "op://vault/…": provider not configured in strict mode`},
		{
			name: "no SOPS reader", provider: SopsProvider(nil, ""),
			body: `{{ readSopsSecret (printf "sops://secrets/db.sops.yaml#/tokens/%s" .item) }}`,
			want: `readSopsSecret "sops://secrets/db.sops.yaml#…"`,
		},
		{name: "reader error in the batch", provider: OpProvider(failingReader), body: fromData, want: `resolving 1password secret "op://vault/…": not found`},
		{name: "reader error on a dynamic ref", provider: OpProvider(failingReader), body: fromUser, want: `resolving 1password secret "op://vault/…": not found`},
		{
			name: "SOPS reader error on a dynamic ref", provider: SopsProvider(failingSopsReader{}, ""),
			body: `{{ readSopsSecret (printf "sops://secrets/db.sops.yaml#/%s" (readSopsSecret "sops://secrets/db.sops.yaml#/user")) }}`,
			want: `decrypting "sops://secrets/db.sops.yaml#…": not found`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := renderTwoPhase(t, tc.provider, tc.body)
			require.ErrorContains(t, err, tc.want)
			assert.NotContains(t, err.Error(), secretItem)
		})
	}
}

// refForError keeps a ref's scheme and its file or vault (share).
func TestRefForError(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string]string{ //nolint:gosec // G101: refs, no credentials
		"sops://secrets/db.sops.yaml#/db/password": "sops://secrets/db.sops.yaml#…",
		"sops://secrets/db.sops.yaml":              "sops://secrets/db.sops.yaml",
		"op://vault/item/field":                    "op://vault/…",
		"pass://share/item/field":                  "pass://share/…",
		"op://vault":                               "op://…",
	} {
		assert.Equal(t, want, refForError(ref), ref)
	}
}
