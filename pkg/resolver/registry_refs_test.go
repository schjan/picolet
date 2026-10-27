package resolver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/sops"
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
		{name: "reader error on a dynamic ref", provider: OpProvider(failingReader), body: fromUser, want: `readOpSecret "op://vault/…": the reader's error is withheld`},
		{
			name: "SOPS reader error on a dynamic ref", provider: SopsProvider(failingSopsReader{}, ""),
			body: `{{ readSopsSecret (printf "sops://secrets/db.sops.yaml#/%s" (readSopsSecret "sops://secrets/db.sops.yaml#/user")) }}`,
			want: `readSopsSecret "sops://secrets/db.sops.yaml#…": the reader's error is withheld`,
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
		// Not a provider ref (a template passed a value): nothing of it.
		"hunter2-TOKEN://x/y":    "…",
		"https://user:pw@host/x": "…",
		"hunter2":                "…",
	} {
		assert.Equal(t, want, refForError(ref), ref)
	}
}

// A provider client quotes the ref it failed with %q, which escapes a quote,
// backslash or non-printable character a template took from a secret value:
// the escaped form is redacted too.
func TestRedactRefsEscapedRef(t *testing.T) {
	t.Parallel()
	for _, item := range []string{`it"em`, `it\em`, "it\tem"} {
		ref := "op://vault/" + item + "/pw"
		err := redactRefs(fmt.Errorf("resolving 1password secret %q: not found", ref), []string{ref})
		require.EqualError(t, err, `resolving 1password secret "op://vault/…": not found`, item)
	}
}

// refForError cuts an op:// or pass:// ref at its vault (share) even when
// its item or field holds a #, which only a sops:// ref uses for a pointer.
func TestRefForErrorHashInProviderRef(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "op://vault/…", refForError("op://vault/it#em/field"))
	assert.Equal(t, "pass://share/…", refForError("pass://share/item/fi#eld"))
}

// A sops:// pointer a template builds at render time from an op:// value
// never reaches the error the real SOPS readers fail it with, though they
// quote the pointer and its failing key apart from the ref: such an error
// names the function and the ref's file only, and still answers errors.Is.
// It holds for readSopsSecret (a cache miss) and readSopsData (a ref the
// collect pass did not read, whether the template skips the call while the
// op:// value is pending or builds a ref from the pending placeholder),
// keyless and decrypting.
func TestSopsErrorsKeepDynamicPointersOut(t *testing.T) {
	t.Parallel()
	const skipPending = `{{ $u := readOpSecret "op://vault/item/user" }}{{ if ne $u "<op-secret-pending>" }}`
	const fromPending = `(printf "sops://secrets/db.sops.yaml#/db/%s" (readOpSecret "op://vault/item/user"))`
	bodies := map[string]string{
		"readSopsSecret skipping the pending value": skipPending + `{{ readSopsSecret (printf "sops://secrets/db.sops.yaml#/db/%s" $u) }}{{ end }}`,
		"readSopsData skipping the pending value":   skipPending + `{{ range readSopsData (printf "sops://secrets/db.sops.yaml#/db/%s" $u) }}{{ . }}{{ end }}{{ end }}`,
		"readSopsData from the pending value":       `{{ range readSopsData ` + fromPending + ` }}{{ . }}{{ end }}`,
	}
	recipient, err := sops.ParseRecipient(testsops.Recipient(t, "srv-1"))
	require.NoError(t, err)
	for name, body := range bodies {
		for mode, newReader := range map[string]func(fs.FS) SopsReader{
			"decrypting": func(fsys fs.FS) SopsReader { return sops.NewReader(fsys, testsops.Identities(t, "srv-1")) },
			"keyless": func(fsys fs.FS) SopsReader {
				return sops.NewKeylessReader(fsys, sops.Host{Name: "test-host", Recipients: []sops.Recipient{recipient}})
			},
		} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				fsys := fstest.MapFS{
					"s.tmpl":               &fstest.MapFile{Data: []byte(body)},
					"secrets/db.sops.yaml": &fstest.MapFile{Data: testsops.CorpusFile(t, "values.sops.yaml")},
				}
				providers := []ProviderTemplate{OpProvider(userReader), SopsProvider(newReader(fsys), "")}
				registry, caches, err := BuildRegistry(t.Context(), fsys, nil, providers, "/var/lib/picolet")
				require.NoError(t, err)
				var discard bytes.Buffer
				_ = registry.ExecuteTemplate(&discard, "s.tmpl", nil) // the collect pass; its errors are swallowed
				require.NoError(t, caches.ResolveAll(t.Context()))
				err = registry.ExecuteTemplate(&discard, "s.tmpl", nil)
				require.ErrorIs(t, err, sops.ErrBadPointer)
				require.ErrorContains(t, err, `"sops://secrets/db.sops.yaml#…"`)
				assert.NotContains(t, err.Error(), secretItem)
			})
		}
	}
}

// When one ref is a prefix of another, the longer one is redacted whole: the
// shorter ref's form never leaves the longer one's item or field behind.
func TestRedactRefsPrefixRef(t *testing.T) {
	t.Parallel()
	refs := []string{"op://vault/db/pass", "op://vault/db/pass-s3cretsuffix"}
	err := redactRefs(fmt.Errorf("resolving 1password secret %q: not found", refs[1]), refs)
	require.EqualError(t, err, `resolving 1password secret "op://vault/…": not found`)
}
