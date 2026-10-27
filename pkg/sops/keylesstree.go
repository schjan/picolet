package sops

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	gsops "github.com/getsops/sops/v3"
)

// keylessFile is a SOPS file as the keyless reader sees it: its keyless view
// (File) and its tree as the Agent's decrypt would walk it, with every value
// the file's encryption rules encrypt replaced by its encrypted marker.
type keylessFile struct {
	*File

	// branches is the tree with each encrypted value replaced by its
	// encrypted marker (encryptedMarker, rendered by leaf); plaintext values
	// keep their decoded value. Nil when
	// walkErr is set.
	branches gsops.TreeBranches
	// walkErr is why the Agent's decrypt fails on the file's structure,
	// before any key is needed (a plaintext value where the rules require
	// ciphertext, an invalid type tag, a non-string key); nil when the walk
	// passes.
	walkErr error
}

// keylessTree walks the SOPS file at path (inspected as f) through
// upstream's tree decrypt with placeholderCipher, so which values count as
// encrypted is decided by the file's encryption rules exactly as in the
// Agent's decrypt. What fails here fails the Agent's decrypt too; what needs
// the key (the MAC, the ciphertext itself) is left to the Agent. It walks a
// tree parsed anew from data (the file's bytes, which Inspect parsed as f)
// with f's store, so f keeps the file as written (walkError and Lookup read
// it).
func keylessTree(path string, f *File, data []byte) (gsops.TreeBranches, error) {
	silenceSOPSLogs()
	tree, err := f.store.LoadEncryptedFile(data)
	if err != nil {
		// Inspect parsed the same bytes with the same store; never pass
		// upstream's text on, it can quote file content.
		return nil, fmt.Errorf("%s: %w: cannot be parsed as %s with sops metadata", path, ErrNotSOPSFile, f.Format)
	}
	c := &placeholderCipher{}
	if _, err := tree.Decrypt(nil, c); err != nil {
		return nil, c.walkError(path, f, err)
	}
	return tree.Branches, nil
}

// cipherFailure is why placeholderCipher refused a value. Its text is a
// fixed token per reason, never the value, so walkError can tell it apart in
// upstream's wrapped error text.
type cipherFailure struct {
	// value is the refused value as the file holds it: for reasonNotEncrypted
	// a plaintext secret. It exists only so walkError can find its pointer;
	// never print it, put it in an error or log it.
	value  string
	reason string
}

func (e *cipherFailure) Error() string { return "picolet-keyless: " + e.reason }

const (
	reasonNotEncrypted = "not encrypted"
	reasonMalformed    = "malformed ciphertext"
	reasonTypeTag      = "invalid type tag"
)

// decryptableTypes are the type tags upstream's cipher decrypts
// (aes/cipher.go); any other fails the Agent's decrypt.
var decryptableTypes = map[string]bool{
	tagStr: true, tagBytes: true, tagInt: true, tagFloat: true, tagBool: true, tagTime: true, tagComment: true,
}

// placeholderCipher is a gsops.Cipher that decrypts nothing: it checks a
// value the way upstream's cipher does before it needs the key (the
// ENC[AES256_GCM,…] format, base64 fields, a known type tag) and returns
// the encryptedMarker of its type tag. It remembers its last refusal for walkError.
type placeholderCipher struct {
	last *cipherFailure
}

func (c *placeholderCipher) Encrypt(any, []byte, string) (string, error) {
	return "", errors.New("placeholderCipher does not encrypt")
}

func (c *placeholderCipher) Decrypt(ciphertext string, _ []byte, _ string) (any, error) {
	c.last = nil
	if ciphertext == "" {
		return "", nil
	}
	m := encRe.FindStringSubmatch(ciphertext)
	if m == nil {
		return nil, c.refuse(ciphertext, reasonNotEncrypted)
	}
	for _, field := range m[1:4] {
		if _, err := base64.StdEncoding.DecodeString(field); err != nil {
			return nil, c.refuse(ciphertext, reasonMalformed)
		}
	}
	if !decryptableTypes[m[4]] {
		return nil, c.refuse(ciphertext, reasonTypeTag)
	}
	if m[4] == tagComment {
		// Upstream's cipher decrypts a comment to a gsops.Comment, which the
		// readers skip; a comment inside a list is stored as a plain list
		// item, so a marker string here would count as an extra element.
		return gsops.Comment{Value: Placeholder}, nil
	}
	return encryptedMarker(m[4]), nil
}

// markerPrefix and markerSuffix frame an encrypted value's type tag in the
// keyless tree. A marker is a string (upstream's walk hashes every value it
// gets back, and only accepts its own types), and the NUL bytes keep it
// apart from any plaintext value a real file holds.
const (
	markerPrefix = "\x00picolet-keyless-encrypted:"
	markerSuffix = "\x00"
)

// encryptedMarker is the keyless tree's value for an encrypted value with
// type tag tag.
func encryptedMarker(tag string) string { return markerPrefix + tag + markerSuffix }

// encryptedTag returns the type tag of the encrypted marker v, false when v
// is not one (a plaintext value).
func encryptedTag(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, markerPrefix) || !strings.HasSuffix(s, markerSuffix) {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(s, markerPrefix), markerSuffix), true
}

// leaf renders the leaf v found at ptr in f's keyless tree. A plaintext
// leaf renders as the decrypting Reader renders it (renderLeaf). An
// encrypted leaf renders as the placeholder of its type, so a template that
// puts it into a typed field (a manifest's replicas:) renders a value the
// field's parser accepts, as the Agent's decrypted value does: an int as
// "0", a float as "0.0", a bool as "false", a time as the zero time, a
// string or bytes value as Placeholder. A direct secrets: entry (direct) is
// opaque bytes no parser reads, so there every encrypted leaf renders as
// Placeholder.
func (f *keylessFile) leaf(v any, ptr Pointer, direct bool) (string, error) {
	tag, ok := encryptedTag(v)
	if !ok {
		return renderLeaf(f.store, v, ptr)
	}
	if direct {
		return Placeholder, nil
	}
	switch tag {
	case tagInt:
		return "0", nil
	case tagFloat:
		return "0.0", nil
	case tagBool:
		return "false", nil
	case tagTime:
		return "0001-01-01T00:00:00Z", nil
	}
	return Placeholder, nil
}

func (c *placeholderCipher) refuse(value, reason string) error {
	c.last = &cipherFailure{value: value, reason: reason}
	return c.last
}

// walkError turns the error of upstream's tree walk over the file at path
// into an ErrEditedOutsideSOPS error that names the offending value's
// pointer (found in f's tree) and never quotes a value or upstream's text.
// Upstream swallows a refusal for a comment, so only a refusal its error
// text carries counts.
func (c *placeholderCipher) walkError(path string, f *File, err error) error {
	last := c.last
	if last == nil || !strings.Contains(err.Error(), last.Error()) {
		return editedError(path, "a value the file's encryption rules require to be encrypted is not a string, or a key is not a string",
			"re-create the file with `sops edit %s`")
	}
	ptr := strconv.Quote(findLeaf(f.tree, last.value))
	switch last.reason {
	case reasonNotEncrypted:
		return editedError(path, ptr+" is not encrypted, though the file's encryption rules require it", "re-create the value with `sops edit %s`")
	case reasonMalformed:
		return editedError(path, ptr+" is not a valid SOPS ciphertext", "re-create the value with `sops edit %s`")
	default:
		return editedError(path, ptr+" has an invalid type tag", "re-create the value with `sops edit %s`")
	}
}

// findLeaf returns the pointer (in RFC 6901 syntax) of the first string leaf
// equal to value in branches' first document holding one, or "(unknown)".
func findLeaf(branches gsops.TreeBranches, value string) string {
	for _, branch := range branches {
		if p, ok := findIn(branch, Pointer{}, value); ok {
			return p.String()
		}
	}
	return "(unknown)"
}

func findIn(node any, at Pointer, value string) (Pointer, bool) {
	switch node := node.(type) {
	case gsops.TreeBranch:
		for _, item := range node {
			key, ok := item.Key.(string)
			if !ok {
				continue
			}
			if p, ok := findIn(item.Value, at.child(key), value); ok {
				return p, true
			}
		}
	case []any:
		i := 0
		for _, item := range node {
			if _, isComment := item.(gsops.Comment); isComment {
				continue
			}
			if p, ok := findIn(item, at.child(strconv.Itoa(i)), value); ok {
				return p, true
			}
			i++
		}
	case string:
		return at, node == value
	}
	return Pointer{}, false
}
