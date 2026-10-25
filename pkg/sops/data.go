package sops

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	gsops "github.com/getsops/sops/v3"
)

// ReadData returns the subtree ref names, the value of the readSopsData
// template helper: a map[string]any for a map, a []any for a list and a
// string for a leaf, nested maps and lists keeping their structure. Without
// a fragment it returns the whole document. A leaf is rendered like a
// readSopsSecret leaf (renderLeaf): a string as is, null as "", another
// scalar as `sops decrypt --extract` prints it minus its trailing newline.
// Leaves are always strings, numbers and booleans too (`42`, `true`),
// so a template comparing or computing with one converts it first. Comments
// are not data and are left out.
//
// It shares ReadAll's per-Reader file cache, so a file both read is
// decrypted once, and its leaves are scrubbed like any decrypted file's.
// Errors name ref and never contain a decrypted value.
func (r *Reader) ReadData(ctx context.Context, raw string) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, plain, err := r.openFile(ctx, raw, r.files)
	if err != nil {
		return nil, err
	}
	node, err := resolvePointer(plain.branches, ref.Pointer)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", raw, err)
	}
	v, err := toData(node, ref.Pointer, func(leaf any, at Pointer) (any, error) {
		return plain.leaf(leaf, at)
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", raw, err)
	}
	return v, nil
}

// ReadData checks ref like ReadAll does, and every leaf of the subtree it
// names like a string ref's leaf, then returns the subtree's real structure
// (key names are cleartext) with every leaf replaced by Placeholder. Errors
// name ref; a subtree with several bad leaves reports each. The file is read
// and inspected anew on each call.
func (r *KeylessReader) ReadData(ctx context.Context, raw string) (any, error) {
	ref, err := ParseRef(raw)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", raw, err)
	}
	f, err := r.inspect(ref.Path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", raw, err)
	}
	if err := checkSOPSFile(f, ref); err != nil {
		return nil, fmt.Errorf("%s: %w", raw, err)
	}
	node, err := resolvePointer(f.tree, ref.Pointer)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", raw, err)
	}
	v, err := toData(node, ref.Pointer, func(leaf any, at Pointer) (any, error) {
		return Placeholder, checkLeaf(ref.Path, at, leaf)
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", raw, err)
	}
	if err := r.checkHost(raw, ref, f); err != nil {
		return nil, err
	}
	return v, nil
}

// toData converts the SOPS tree node found at ptr into template data: maps
// to map[string]any, lists to []any, and each leaf through leaf. Comments
// are skipped. Every leaf is visited; the leaf errors are joined.
func toData(node any, ptr Pointer, leaf func(v any, at Pointer) (any, error)) (any, error) {
	switch node := node.(type) {
	case gsops.TreeBranch:
		m := make(map[string]any, len(node))
		var errs []error
		for _, item := range node {
			if _, isComment := item.Key.(gsops.Comment); isComment {
				continue
			}
			key := fmt.Sprint(item.Key)
			v, err := toData(item.Value, ptr.child(key), leaf)
			errs = append(errs, err)
			m[key] = v
		}
		return m, errors.Join(errs...)
	case []any:
		list := make([]any, 0, len(node))
		var errs []error
		for _, item := range node {
			if _, isComment := item.(gsops.Comment); isComment {
				continue
			}
			v, err := toData(item, ptr.child(strconv.Itoa(len(list))), leaf)
			errs = append(errs, err)
			list = append(list, v)
		}
		return list, errors.Join(errs...)
	}
	return leaf(node, ptr)
}

// child returns the pointer to p's child seg.
func (p Pointer) child(seg string) Pointer {
	segs := make([]string, len(p.segments), len(p.segments)+1)
	copy(segs, p.segments)
	return Pointer{segments: append(segs, seg)}
}
