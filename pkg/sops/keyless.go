package sops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"
)

// Placeholder is what the keyless reader renders for a whole file, an
// encrypted string or bytes leaf, and any encrypted leaf of a direct
// secrets: entry. An encrypted int, float, bool or time leaf renders as a
// value of its type instead (keylessFile.leaf).
const Placeholder = "<sops-secret>"

// Host is the Host a KeylessReader checks refs for: its name and the age
// recipients its host.yml declares (age_recipients:).
type Host struct {
	Name       string
	Recipients []Recipient
}

// KeylessReader checks sops:// refs against a Fleet FS without any key and
// renders Placeholder for each whole file and encrypted leaf that passes (an
// encrypted int, float, bool or time leaf: a value of its type, see
// keylessFile.leaf); a plaintext leaf (one the file's encryption rules leave unencrypted, "" and
// null included) renders as the decrypting Reader renders it. It is the
// reader of `validate`, `resolve` and bootstrap's strict render: it reads no
// age identity from anywhere, environment variables included.
//
// It never fails a per-ref read the decrypting Reader accepts: what it
// cannot see without a key (MAC, ciphertext) it leaves to the Agent.
//
// Besides the file and pointer checks, it checks the Host's recipients
// against each file's recipient labels: the Host must declare
// age_recipients:, and appear in enough key groups. The labels are
// unauthenticated, so this catches drift, not a hostile edit.
//
// One KeylessReader serves one resolve: each file is read and inspected at
// most once, whichever calls name it.
type KeylessReader struct {
	fsys fs.FS
	host Host

	mu    sync.Mutex
	files map[string]inspected
}

// NewKeylessReader returns a KeylessReader over fsys (the Fleet root) that
// checks refs for host.
func NewKeylessReader(fsys fs.FS, host Host) *KeylessReader {
	return &KeylessReader{fsys: fsys, host: host, files: map[string]inspected{}}
}

// ReadAll checks refs and returns the keyless value of each ref that passes
// (Placeholder, or a plaintext leaf's value), plus one joined error carrying
// every failed ref's error (each names its ref), the shape of
// Reader.ReadAll.
func (r *KeylessReader) ReadAll(ctx context.Context, refs []string) (map[string]string, error) {
	return r.readAll(ctx, refs, false)
}

// ReadDirect is ReadAll for direct `secrets:` refs, which need a non-empty
// value: an "" or null leaf (which SOPS leaves unencrypted, so it shows
// without a key) fails with ErrEmptyValue, as on the Agent.
func (r *KeylessReader) ReadDirect(ctx context.Context, refs []string) (map[string]string, error) {
	return r.readAll(ctx, refs, true)
}

func (r *KeylessReader) readAll(ctx context.Context, refs []string, direct bool) (map[string]string, error) {
	values := make(map[string]string, len(refs))
	var errs []error
	for _, raw := range refs {
		v, err := r.check(ctx, raw, direct)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		values[raw] = v
	}
	return values, errors.Join(errs...)
}

type inspected struct {
	file *keylessFile
	err  error
}

// check checks the string ref raw and returns its keyless value.
func (r *KeylessReader) check(ctx context.Context, raw string, direct bool) (string, error) {
	ref, err := ParseRef(raw)
	if err != nil {
		return "", err
	}
	f, err := r.file(ctx, ref.Path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", raw, err)
	}
	v, err := checkRef(f, ref, direct)
	if err != nil {
		return "", fmt.Errorf("%s: %w", raw, err)
	}
	if direct && v == "" {
		return "", EmptyDirectValueError(raw)
	}
	return v, r.checkHost(raw, ref, f.File)
}

// file returns the inspected file at path, reading it on the first call
// (unless ctx is done) and from the reader's cache after.
func (r *KeylessReader) file(ctx context.Context, path string) (*keylessFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if res, ok := r.files[path]; ok {
		return res.file, res.err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var res inspected
	res.file, res.err = r.inspect(path)
	r.files[path] = res
	return res.file, res.err
}

// checkHost checks the Host's recipients against f, the file ref (written
// raw) names: the Host must declare age_recipients: and appear in enough of
// f's key groups.
func (r *KeylessReader) checkHost(raw string, ref Ref, f *File) error {
	if len(r.host.Recipients) == 0 {
		return fmt.Errorf("Host %s references %s but declares no age_recipients; add its age public key to age_recipients: in its host.yml", r.host.Name, raw)
	}
	if err := checkRecipients(ref.Path, f, "Host "+r.host.Name, r.host.Recipients); err != nil {
		return fmt.Errorf("%s: %w", raw, err)
	}
	return nil
}

// checkRef checks what a string ref names in f and returns its keyless
// value: Placeholder for the whole file, else the leaf the fragment names
// rendered from f's keyless tree (keylessFile.leaf; direct for a direct
// secrets: entry). f must be a SOPS file whose tree passes the keyless walk.
// Errors never quote a value.
func checkRef(f *keylessFile, ref Ref, direct bool) (string, error) {
	if err := checkKeylessFile(f, ref); err != nil {
		return "", err
	}
	if ref.WholeFile() {
		return Placeholder, nil
	}
	v, err := resolvePointer(f.branches, ref.Pointer)
	if err != nil {
		return "", err
	}
	ptr := ref.Pointer.String()
	switch describe(v).Kind {
	case NodeMap:
		return "", fmt.Errorf("%w: %q is a map; a string ref must end at a leaf", ErrNotLeaf, ptr)
	case NodeList:
		return "", fmt.Errorf("%w: %q is a list; a string ref must end at a leaf", ErrNotLeaf, ptr)
	case NodeLeaf:
	}
	return f.leaf(v, ref.Pointer, direct)
}

// checkKeylessFile fails when f, the file ref names, is not a SOPS file or
// its tree fails the keyless walk (keylessTree), as the Agent's decrypt
// would.
func checkKeylessFile(f *keylessFile, ref Ref) error {
	if err := checkSOPSFile(f.File, ref); err != nil {
		return err
	}
	return f.walkErr
}

// checkSOPSFile fails when f, the file ref names, is not a SOPS file.
func checkSOPSFile(f *File, ref Ref) error {
	if !f.IsSOPS {
		return fmt.Errorf("%s: %w: %s; encrypt it with `sops encrypt -i %s`", ref.Path, ErrNotSOPSFile, f.NotSOPSReason, ref.Path)
	}
	return nil
}

// EmptyDirectValueError is the ErrEmptyValue error of the direct `secrets:`
// ref raw: its value is empty (an "" or null leaf).
func EmptyDirectValueError(raw string) error {
	return fmt.Errorf("%s: %w: a direct secrets: entry needs a non-empty value (the leaf is \"\" or null)", raw, ErrEmptyValue)
}

// inspect reads and inspects the file at path and, for a SOPS file, walks
// its tree keyless (keylessTree).
func (r *KeylessReader) inspect(path string) (*keylessFile, error) {
	data, err := ReadFile(r.fsys, path)
	if err != nil {
		return nil, err
	}
	f, err := Inspect(path, data)
	if err != nil {
		return nil, err
	}
	kf := &keylessFile{File: f}
	if f.IsSOPS {
		kf.branches, kf.walkErr = keylessTree(path, data, f)
	}
	return kf, nil
}
