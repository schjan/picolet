package sops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	gsops "github.com/getsops/sops/v3"
	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/require"
)

// TestInspectCorpusGolden snapshots the keyless inspection of every corpus
// file: metadata, recipients per key group, and every leaf's pointer, kind,
// encryption and type. Ciphertexts and timestamps are not part of it, so
// regenerating the corpus (`task sops:corpus`) must leave these goldens
// unchanged; a SOPS upgrade that changes behaviour shows up as a diff.
// Refresh with -update.
func TestInspectCorpusGolden(t *testing.T) {
	t.Parallel()

	dir := filepath.Join("..", "..", "testdata", "sops", "corpus")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	g := goldie.New(t, goldie.WithFixtureDir(filepath.Join("testdata", "inspect")), goldie.WithNameSuffix(".golden.json"))
	for _, e := range entries {
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := ReadFile(os.DirFS(dir), name)
			require.NoError(t, err)
			f, err := Inspect(name, data)
			require.NoError(t, err)

			type leaf struct {
				Pointer   string   `json:"pointer"`
				Kind      NodeKind `json:"kind"`
				Encrypted bool     `json:"encrypted,omitempty"`
				Type      string   `json:"type,omitempty"`
			}
			var leaves []leaf
			if f.IsSOPS {
				walkNodes(f.tree[0], Pointer{}, func(p Pointer, v any) {
					got, err := f.Lookup(p)
					require.NoError(t, err, "Lookup of a walked pointer")
					require.Equal(t, describe(v), got, "Lookup of a walked pointer")
					leaves = append(leaves, leaf{Pointer: p.String(), Kind: got.Kind, Encrypted: got.Encrypted, Type: got.Type})
				})
			}
			out, err := json.MarshalIndent(struct {
				File   *File  `json:"file"`
				Leaves []leaf `json:"nodes,omitempty"`
			}{f, leaves}, "", "  ")
			require.NoError(t, err)
			g.Assert(t, name, append(out, '\n'))
		})
	}
}

// walkNodes calls fn for every node below the root in document order with its
// pointer.
func walkNodes(v any, at Pointer, fn func(Pointer, any)) {
	child := func(seg string) Pointer {
		return Pointer{segments: append(append([]string(nil), at.segments...), seg)}
	}
	switch node := v.(type) {
	case gsops.TreeBranch:
		for _, item := range node {
			k, ok := item.Key.(string)
			if !ok {
				continue // comment
			}
			p := child(k)
			fn(p, item.Value)
			walkNodes(item.Value, p, fn)
		}
	case []any:
		i := 0
		for _, item := range node {
			if _, isComment := item.(gsops.Comment); isComment {
				continue
			}
			p := child(strconv.Itoa(i))
			fn(p, item)
			walkNodes(item, p, fn)
			i++
		}
	}
}
