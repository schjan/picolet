package sops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
)

// Placeholder is what the keyless reader renders for every sops:// ref.
const Placeholder = "<sops-secret>"

// Host is the Host a KeylessReader checks refs for: its name and the age
// recipients its host.yml declares (age_recipients:).
type Host struct {
	Name       string
	Recipients []Recipient
}

// KeylessReader checks sops:// refs against a Fleet FS without any key and
// renders Placeholder for each one that passes. It is the reader of
// `validate`, `resolve` and bootstrap's strict render: it reads no age
// identity from anywhere, environment variables included.
//
// Besides the file and pointer checks, it checks the Host's recipients
// against each file's recipient labels: the Host must declare
// age_recipients:, and appear in enough key groups. The labels are
// unauthenticated, so this catches drift, not a hostile edit.
type KeylessReader struct {
	fsys fs.FS
	host Host
}

// NewKeylessReader returns a KeylessReader over fsys (the Fleet root) that
// checks refs for host.
func NewKeylessReader(fsys fs.FS, host Host) *KeylessReader {
	return &KeylessReader{fsys: fsys, host: host}
}

// ReadAll checks refs and returns Placeholder for each ref that passes, plus
// one joined error carrying every failed ref's error (each names its ref),
// the shape of Reader.ReadAll. Each file is read and inspected at most once
// per call.
func (r *KeylessReader) ReadAll(ctx context.Context, refs []string) (map[string]string, error) {
	return r.readAll(ctx, refs, false)
}

// ReadDirect is ReadAll for direct `secrets:` refs, which need a non-empty
// value: an "" or null leaf (which SOPS leaves unencrypted, so it shows
// without a key) fails with ErrEmptyValue.
func (r *KeylessReader) ReadDirect(ctx context.Context, refs []string) (map[string]string, error) {
	return r.readAll(ctx, refs, true)
}

func (r *KeylessReader) readAll(ctx context.Context, refs []string, direct bool) (map[string]string, error) {
	values := make(map[string]string, len(refs))
	files := map[string]inspected{}
	var errs []error
	for _, raw := range refs {
		if err := r.check(ctx, raw, files, direct); err != nil {
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

func (r *KeylessReader) check(ctx context.Context, raw string, files map[string]inspected, direct bool) error {
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
	if direct && isEmptyLeaf(res.file, ref) {
		return EmptyDirectValueError(raw)
	}
	if len(r.host.Recipients) == 0 {
		return fmt.Errorf("Host %s references %s but declares no age_recipients; add its age public key to age_recipients: in its host.yml", r.host.Name, raw)
	}
	if err := checkRecipients(ref.Path, res.file, "Host "+r.host.Name, r.host.Recipients); err != nil {
		return fmt.Errorf("%s: %w", raw, err)
	}
	return nil
}

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

// isEmptyLeaf reports whether ref (already checked by checkRef) names an ""
// or null leaf of f.
func isEmptyLeaf(f *File, ref Ref) bool {
	if ref.WholeFile() {
		return false
	}
	v, err := resolvePointer(f.tree, ref.Pointer)
	return err == nil && (v == nil || v == "")
}

// EmptyDirectValueError is the ErrEmptyValue error of the direct `secrets:`
// ref raw: its value is empty (an "" or null leaf).
func EmptyDirectValueError(raw string) error {
	return fmt.Errorf("%s: %w: a direct secrets: entry needs a non-empty value (the leaf is \"\" or null)", raw, ErrEmptyValue)
}

func (r *KeylessReader) inspect(path string) (*File, error) {
	data, err := ReadFile(r.fsys, path)
	if err != nil {
		return nil, err
	}
	return Inspect(path, data)
}
