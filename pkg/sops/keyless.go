package sops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
)

// Placeholder is what the keyless reader renders for every sops:// ref.
const Placeholder = "<sops-secret>"

// KeylessReader checks sops:// refs against a Fleet FS without any key and
// renders Placeholder for each one that passes. It is the reader of
// `validate`, `resolve` and bootstrap's strict render: it reads no age
// identity from anywhere, environment variables included.
type KeylessReader struct {
	fsys fs.FS
}

// NewKeylessReader returns a KeylessReader over fsys (the Fleet root).
func NewKeylessReader(fsys fs.FS) *KeylessReader {
	return &KeylessReader{fsys: fsys}
}

// ReadAll checks refs and returns Placeholder for each ref that passes, plus
// one joined error carrying every failed ref's error (each names its ref),
// the shape of Reader.ReadAll. Each file is read and inspected at most once
// per call.
func (r *KeylessReader) ReadAll(ctx context.Context, refs []string) (map[string]string, error) {
	values := make(map[string]string, len(refs))
	files := map[string]inspected{}
	var errs []error
	for _, raw := range refs {
		if err := r.check(ctx, raw, files); err != nil {
			errs = append(errs, err)
			continue
		}
		values[raw] = Placeholder
	}
	return values, errors.Join(errs...)
}

type inspected struct {
	file *File
	err  error
}

func (r *KeylessReader) check(ctx context.Context, raw string, files map[string]inspected) error {
	ref, err := ParseRef(raw)
	if err != nil {
		return err
	}
	res, ok := files[ref.Path]
	if !ok {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: %w", raw, err)
		}
		res.file, res.err = r.inspect(ref.Path)
		files[ref.Path] = res
	}
	if res.err != nil {
		return fmt.Errorf("%s: %w", raw, res.err)
	}
	if err := checkRef(res.file, ref); err != nil {
		return fmt.Errorf("%s: %w", raw, err)
	}
	return nil
}

// validTypeTags are the type tags SOPS's cipher decrypts a value as.
var validTypeTags = map[string]bool{"str": true, "int": true, "float": true, "bool": true, "time": true, "bytes": true}

// checkRef checks what a string ref names in f: a SOPS file, and for a
// fragment a leaf that is encrypted with a valid type tag, or an "" or null
// leaf (SOPS never encrypts those). Errors never quote a value.
func checkRef(f *File, ref Ref) error {
	if !f.IsSOPS {
		return fmt.Errorf("%s: %w: %s; encrypt it with `sops encrypt -i %s`", ref.Path, ErrNotSOPSFile, f.NotSOPSReason, ref.Path)
	}
	if ref.WholeFile() {
		return nil
	}
	v, err := resolvePointer(f.tree, ref.Pointer)
	if err != nil {
		return err
	}
	ptr := ref.Pointer.String()
	node := describe(v)
	switch {
	case node.Kind == NodeMap:
		return fmt.Errorf("%w: %q is a map; a string ref must end at a leaf", ErrNotLeaf, ptr)
	case node.Kind == NodeList:
		return fmt.Errorf("%w: %q is a list; a string ref must end at a leaf", ErrNotLeaf, ptr)
	case v == nil || v == "":
		return nil
	case !node.Encrypted:
		return fmt.Errorf("%s: %q is not encrypted (the file's encryption rules leave it in plaintext); a sops:// ref must name an encrypted value", ref.Path, ptr)
	case !validTypeTags[node.Type]:
		return fmt.Errorf("%s: %q has an invalid type tag; restore the file from git, or re-create the value with `sops edit %s`", ref.Path, ptr, ref.Path)
	}
	return nil
}

func (r *KeylessReader) inspect(path string) (*File, error) {
	data, err := ReadFile(r.fsys, path)
	if err != nil {
		return nil, err
	}
	return Inspect(path, data)
}
