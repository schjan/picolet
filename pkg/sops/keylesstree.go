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
// the file's encryption rules encrypt replaced by Placeholder.
type keylessFile struct {
	*File

	// branches is the tree with each encrypted value replaced by
	// Placeholder; plaintext values keep their decoded value. Nil when
	// walkErr is set.
	branches gsops.TreeBranches
	// walkErr is why the Agent's decrypt fails on the file's structure,
	// before any key is needed (a plaintext value where the rules require
	// ciphertext, an invalid type tag, a non-string key); nil when the walk
	// passes.
	walkErr error
}

// keylessTree walks the SOPS file at path (data, inspected as f) through
// upstream's tree decrypt with placeholderCipher, so which values count as
// encrypted is decided by the file's encryption rules exactly as in the
// Agent's decrypt. What fails here fails the Agent's decrypt too; what needs
// the key (the MAC, the ciphertext itself) is left to the Agent.
func keylessTree(path string, data []byte, f *File) (gsops.TreeBranches, error) {
	silenceSOPSLogs()
	tree, err := f.store.LoadEncryptedFile(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: cannot be parsed", path, ErrNotSOPSFile)
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
	tagStr: true, tagBytes: true, tagInt: true, tagFloat: true, tagBool: true, tagTime: true, "comment": true,
}

// placeholderCipher is a gsops.Cipher that decrypts nothing: it checks a
// value the way upstream's cipher does before it needs the key (the
// ENC[AES256_GCM,…] format, base64 fields, a known type tag) and returns
// Placeholder for it. It remembers its last refusal for walkError.
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
