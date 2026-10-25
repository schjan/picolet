package sops

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"time"

	gsops "github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	"github.com/getsops/sops/v3/keyservice"
	"github.com/getsops/sops/v3/logging"
)

var (
	// ErrNotRecipient is returned (wrapped) when a file is not encrypted to
	// any of this Host's identities (or to too few of its key groups).
	ErrNotRecipient = errors.New("not a recipient")
	// ErrEditedOutsideSOPS is returned (wrapped) when a file fails SOPS's
	// integrity checks: its MAC does not match, a value does not decrypt, or
	// the age stanza labelled with this Host's recipient does not open.
	ErrEditedOutsideSOPS = errors.New("edited outside sops")
	// ErrNotLeaf is returned (wrapped) when a string ref's pointer ends at a
	// map or a list.
	ErrNotLeaf = errors.New("not a leaf")
	// ErrEmptyValue is returned (wrapped) when a direct secrets: ref names an
	// "" or null leaf: a Podman secret needs a non-empty value.
	ErrEmptyValue = errors.New("empty value")
)

// Reader decrypts sops:// refs from a Fleet FS with explicit age identities.
// It never reads a key from anywhere else: no environment variable, no file
// in the home directory, no command, no plugin, no KMS and no network.
//
// A Reader serves one resolve: it keeps every file it decrypted (and every
// file that failed) for its lifetime, so each file is decrypted at most once
// however many ReadAll calls reach it, and it remembers the decrypted values
// so the caller can scrub them from error text (Scrub). Create a new Reader
// per resolve; never keep one in a package variable or across resolves.
type Reader struct {
	fsys fs.FS
	ids  *Identities

	mu     sync.Mutex
	files  map[string]fileResult
	values map[string]struct{} // decrypted leaf values, for Scrub
}

// NewReader returns a Reader over fsys (the Fleet root) that decrypts with
// ids. With nil ids every ref fails with ErrNoIdentity.
//
// The first call discards the output of SOPS's package-global loggers for
// the whole process (see silenceSOPSLogs); picolet never wants their output.
func NewReader(fsys fs.FS, ids *Identities) *Reader {
	silenceSOPSLogs()
	return &Reader{fsys: fsys, ids: ids, files: map[string]fileResult{}, values: map[string]struct{}{}}
}

// ReadAll resolves refs and returns the values of those that resolved plus
// one joined error carrying every failed ref's error (each names its ref),
// the shape of the other providers' readers. Each file is decrypted at most
// once per Reader. Errors never contain a decrypted value or a hash of one.
func (r *Reader) ReadAll(ctx context.Context, refs []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	values := make(map[string]string, len(refs))
	var errs []error
	for _, raw := range refs {
		v, err := r.read(ctx, raw, r.files)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		values[raw] = v
	}
	return values, errors.Join(errs...)
}

// DecryptedFiles returns how many files this Reader decrypted successfully;
// 0 for a nil Reader.
func (r *Reader) DecryptedFiles() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, res := range r.files {
		if res.err == nil {
			n++
		}
	}
	return n
}

type fileResult struct {
	plain *plainFile
	err   error
}

func (r *Reader) read(ctx context.Context, raw string, files map[string]fileResult) (string, error) {
	ref, plain, err := r.openFile(ctx, raw, files)
	if err != nil {
		return "", err
	}
	v, err := plain.extract(ref)
	if err != nil {
		return "", fmt.Errorf("%s: %w", raw, err)
	}
	return v, nil
}

// openFile parses raw and returns the decrypted file it names, decrypting
// it on first use and caching the result (or the failure) in files. Errors
// name raw.
func (r *Reader) openFile(ctx context.Context, raw string, files map[string]fileResult) (Ref, *plainFile, error) {
	ref, err := ParseRef(raw)
	if err != nil {
		return Ref{}, nil, err
	}
	if r.ids.Len() == 0 {
		return Ref{}, nil, fmt.Errorf("%s: %w; %s", raw, ErrNoIdentity, NoIdentityHint)
	}
	res, ok := files[ref.Path]
	if !ok {
		if err := ctx.Err(); err != nil {
			return Ref{}, nil, fmt.Errorf("%s: %w", raw, err)
		}
		res.plain, res.err = r.decryptFile(ref.Path)
		files[ref.Path] = res
		if res.err == nil {
			res.plain.collectLeaves(r.values)
		}
	}
	if res.err != nil {
		return Ref{}, nil, fmt.Errorf("%s: %w", raw, res.err)
	}
	return ref, res.plain, nil
}

// plainFile is a decrypted SOPS file and the store that renders it.
type plainFile struct {
	store    sopsStore
	branches gsops.TreeBranches
}

func (r *Reader) decryptFile(path string) (*plainFile, error) {
	data, err := ReadFile(r.fsys, path)
	if err != nil {
		return nil, err
	}
	return decrypt(path, data, r.ids)
}

// decrypt decrypts a SOPS file with ids through upstream's tree decrypt and
// verifies the MAC the way upstream's cmd/sops/common.DecryptTree does (with
// IgnoreMac false); keep the two in step on a SOPS upgrade. It mirrors
// DecryptTree rather than calling it because upstream error strings are never
// passed on: a type tag mismatch embeds the plaintext, and a MAC mismatch
// prints a hash of it.
func decrypt(path string, data []byte, ids *Identities) (*plainFile, error) {
	f, err := Inspect(path, data)
	if err != nil {
		return nil, err
	}
	if !f.IsSOPS {
		return nil, fmt.Errorf("%s: %w: %s", path, ErrNotSOPSFile, f.NotSOPSReason)
	}
	// The recipient pre-check names "not a recipient" exactly; past it, the
	// decrypt result decides.
	if err := checkRecipients(path, f, "this Host", ids.recipients); err != nil {
		return nil, err
	}
	_, st := storeFor(path)
	tree, err := st.LoadEncryptedFile(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: cannot be parsed", path, ErrNotSOPSFile)
	}
	key, err := tree.Metadata.GetDataKeyWithKeyServices(
		[]keyservice.KeyServiceClient{ageKeyService{ids: ids.ids}}, []string{"age"})
	if err != nil {
		return nil, editedError(path, "its age stanza for this Host's recipient does not open with this Host's identity", "re-encrypt it with `sops updatekeys %s`")
	}
	cipher := aes.NewCipher()
	computedMAC, err := tree.Decrypt(key, cipher)
	if err != nil {
		return nil, editedError(path, "a value does not decrypt", "re-create the value with `sops edit %s`")
	}
	fileMAC, err := cipher.Decrypt(tree.Metadata.MessageAuthenticationCode, key, tree.Metadata.LastModified.Format(time.RFC3339))
	if s, ok := fileMAC.(string); err != nil || !ok || subtle.ConstantTimeCompare([]byte(s), []byte(computedMAC)) != 1 {
		return nil, editedError(path, "its MAC does not match its content", "edit values only with `sops edit %s`")
	}
	return &plainFile{store: st, branches: tree.Branches}, nil
}

// checkRecipients returns an ErrNotRecipient error for the file at path when
// recipients (who's: "this Host" or "Host <name>") appear in fewer of f's key
// groups than it needs. It compares the file's unauthenticated labels.
func checkRecipients(path string, f *File, who string, recipients []Recipient) error {
	matched, required := matchedGroups(f, recipients), f.RequiredGroups()
	if matched >= required {
		return nil
	}
	reach := "the file is not encrypted to " + who
	if matched > 0 {
		reach = fmt.Sprintf("the file is encrypted to %s in %d of the %d key groups it needs", who, matched, required)
	}
	return fmt.Errorf("%s: %w: %s (%s's recipients: %s); add one of them to the file's .sops.yaml creation rule and run `sops updatekeys %s`",
		path, ErrNotRecipient, reach, who, strings.Join(RecipientStrings(recipients), ", "), path)
}

// editedError builds an ErrEditedOutsideSOPS error for path: what failed,
// then the shared remediation ("restore the file from git, or ...") with
// alternative, a format whose one %s is the path.
func editedError(path, what, alternative string) error {
	return fmt.Errorf("%s: %w: %s; restore the file from git, or "+alternative,
		path, ErrEditedOutsideSOPS, what, path)
}

// matchedGroups counts the key groups that list one of recipients.
func matchedGroups(f *File, recipients []Recipient) int {
	n := 0
	for _, g := range f.KeyGroups {
		for _, r := range g.Age {
			if slices.Contains(recipients, r) {
				n++
				break
			}
		}
	}
	return n
}

// extract renders what ref names: the whole file as `sops decrypt` prints it,
// or a leaf as `sops decrypt --extract` prints it. A null leaf renders empty.
func (p *plainFile) extract(ref Ref) (string, error) {
	if ref.WholeFile() {
		out, err := p.store.EmitPlainFile(p.branches)
		if err != nil {
			return "", errors.New("the decrypted file cannot be rendered")
		}
		return string(out), nil
	}
	v, err := resolvePointer(p.branches, ref.Pointer)
	if err != nil {
		return "", err
	}
	switch v.(type) {
	case gsops.TreeBranch:
		return "", fmt.Errorf("%w: %q is a map; a string ref must end at a leaf", ErrNotLeaf, ref.Pointer.String())
	case []any:
		return "", fmt.Errorf("%w: %q is a list; a string ref must end at a leaf", ErrNotLeaf, ref.Pointer.String())
	}
	return p.leaf(v, ref.Pointer)
}

// leaf renders the leaf v found at ptr (see renderLeaf).
func (p *plainFile) leaf(v any, ptr Pointer) (string, error) {
	return renderLeaf(p.store, v, ptr)
}

// renderLeaf renders the plaintext leaf v found at ptr as
// `sops decrypt --extract` prints it, through st: a string as is, null as
// "", and another scalar (int, bool, float, time) minus exactly one trailing
// newline, which the store's emitter appends. The one leaf rule of
// readSopsSecret, readSopsData and direct secrets: refs, keyless and
// decrypting alike.
func renderLeaf(st sopsStore, v any, ptr Pointer) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case nil:
		return "", nil
	}
	out, err := st.EmitValue(v)
	if err != nil {
		return "", fmt.Errorf("the value at %q cannot be rendered", ptr.String())
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

var silenceOnce sync.Once

// silenceSOPSLogs discards the output of SOPS's package loggers, which may
// log key material locations or values at debug and warn level.
func silenceSOPSLogs() {
	silenceOnce.Do(func() {
		for _, l := range logging.Loggers {
			l.SetOutput(io.Discard)
		}
	})
}
