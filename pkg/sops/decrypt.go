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
)

// Reader decrypts sops:// refs from a Fleet FS with explicit age identities.
// It never reads a key from anywhere else: no environment variable, no file
// in the home directory, no command, no plugin, no KMS and no network.
type Reader struct {
	fsys fs.FS
	ids  *Identities
}

// NewReader returns a Reader over fsys (the Fleet root) that decrypts with
// ids. With nil ids every ref fails with ErrNoIdentity.
//
// The first call discards the output of SOPS's package-global loggers for
// the whole process (see silenceSOPSLogs); picolet never wants their output.
func NewReader(fsys fs.FS, ids *Identities) *Reader {
	silenceSOPSLogs()
	return &Reader{fsys: fsys, ids: ids}
}

// ReadAll resolves refs and returns the values of those that resolved plus
// one joined error carrying every failed ref's error (each names its ref),
// the shape of the other providers' readers. Each file is decrypted at most
// once per call. Errors never contain a decrypted value or a hash of one.
func (r *Reader) ReadAll(ctx context.Context, refs []string) (map[string]string, error) {
	values := make(map[string]string, len(refs))
	files := map[string]fileResult{}
	var errs []error
	for _, raw := range refs {
		v, err := r.read(ctx, raw, files)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		values[raw] = v
	}
	return values, errors.Join(errs...)
}

type fileResult struct {
	plain *plainFile
	err   error
}

func (r *Reader) read(ctx context.Context, raw string, files map[string]fileResult) (string, error) {
	ref, err := ParseRef(raw)
	if err != nil {
		return "", err
	}
	if r.ids.Len() == 0 {
		return "", fmt.Errorf("%s: %w: set sops.age_identity_path in the Agent config", raw, ErrNoIdentity)
	}
	res, ok := files[ref.Path]
	if !ok {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("%s: %w", raw, err)
		}
		res.plain, res.err = r.decryptFile(ref.Path)
		files[ref.Path] = res
	}
	if res.err != nil {
		return "", fmt.Errorf("%s: %w", raw, res.err)
	}
	v, err := res.plain.extract(ref)
	if err != nil {
		return "", fmt.Errorf("%s: %w", raw, err)
	}
	return v, nil
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
	_, st := storeFor(path)
	tree, err := st.LoadEncryptedFile(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: cannot be parsed", path, ErrNotSOPSFile)
	}
	key, err := tree.Metadata.GetDataKeyWithKeyServices(
		[]keyservice.KeyServiceClient{ageKeyService{ids: ids.ids}}, []string{"age"})
	if err != nil {
		return nil, dataKeyError(path, f, ids)
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

// dataKeyError names why the data key could not be recovered. The recipient
// pre-check decides the class: a file whose labels don't name this Host in
// enough key groups is "not a recipient"; one whose labels do, yet whose
// stanzas don't open, was edited outside sops.
func dataKeyError(path string, f *File, ids *Identities) error {
	if err := checkRecipients(path, f, "this Host", ids.recipients); err != nil {
		return err
	}
	return editedError(path, "its age stanza for this Host's recipient does not open with this Host's identity", "re-encrypt it with `sops updatekeys %s`")
}

// checkRecipients returns an ErrNotRecipient error for the file at path when
// recipients (who's: "this Host" or "Host <name>") appear in fewer of f's key
// groups than it needs. It compares the file's unauthenticated labels.
func checkRecipients(path string, f *File, who string, recipients []Recipient) error {
	matched, required := matchedGroups(f, recipients), f.RequiredGroups()
	if matched >= required {
		return nil
	}
	labels := make([]string, 0, len(recipients))
	for _, r := range recipients {
		labels = append(labels, r.String())
	}
	reach := "the file is not encrypted to " + who
	if matched > 0 {
		reach = fmt.Sprintf("the file is encrypted to %s in %d of the %d key groups it needs", who, matched, required)
	}
	return fmt.Errorf("%s: %w: %s (%s's recipients: %s); add one of them to the file's .sops.yaml creation rule and run `sops updatekeys %s`",
		path, ErrNotRecipient, reach, who, strings.Join(labels, ", "), path)
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
	switch v := v.(type) {
	case gsops.TreeBranch:
		return "", fmt.Errorf("%w: %q is a map; a string ref must end at a leaf", ErrNotLeaf, ref.Pointer.String())
	case []any:
		return "", fmt.Errorf("%w: %q is a list; a string ref must end at a leaf", ErrNotLeaf, ref.Pointer.String())
	case string:
		return v, nil
	case nil:
		return "", nil
	}
	out, err := p.store.EmitValue(v)
	if err != nil {
		return "", fmt.Errorf("the value at %q cannot be rendered", ref.Pointer.String())
	}
	return string(out), nil
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
