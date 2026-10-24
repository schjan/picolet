// Package sopsage is a throwaway prototype: an age-only SOPS decryptor for
// YAML (and JSON, which is parsed as YAML) files, without the getsops module.
// It mirrors sops v3.13.3: sops.go (Tree.Decrypt, shouldBeEncrypted, ToBytes),
// aes/cipher.go (value format, AES-256-GCM with 32-byte IV), age/keysource.go
// (armored age-wrapped data key) and stores/stores.go (metadata layout).
//
// Not supported (returns an error): Shamir key groups with threshold > 1,
// non-age master keys only, dotenv/INI/binary stores, time values.
package sopsage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
	"go.yaml.in/yaml/v4"
)

// macOnlyEncryptedInit is sops.MACOnlyEncryptedInitialization.
var macOnlyEncryptedInit = []byte{0x8a, 0x3f, 0xd2, 0xad, 0x54, 0xce, 0x66, 0x52, 0x7b, 0x10, 0x34, 0xf3, 0xd1, 0x47, 0xbe, 0xb, 0xb, 0x97, 0x5b, 0x3b, 0xf4, 0x4f, 0x72, 0xc6, 0xfd, 0xad, 0xec, 0x81, 0x76, 0xf2, 0x7d, 0x69}

var encRe = regexp.MustCompile(`^ENC\[AES256_GCM,data:(.+),iv:(.+),tag:(.+),type:(.+)\]`)

type ageKey struct {
	Recipient string `yaml:"recipient"`
	Enc       string `yaml:"enc"`
}

type keyGroup struct {
	Age []ageKey `yaml:"age"`
}

type metadata struct {
	KeyGroups               []keyGroup `yaml:"key_groups"`
	ShamirThreshold         int        `yaml:"shamir_threshold"`
	Age                     []ageKey   `yaml:"age"`
	LastModified            string     `yaml:"lastmodified"`
	MAC                     string     `yaml:"mac"`
	UnencryptedSuffix       string     `yaml:"unencrypted_suffix"`
	EncryptedSuffix         string     `yaml:"encrypted_suffix"`
	UnencryptedRegex        string     `yaml:"unencrypted_regex"`
	EncryptedRegex          string     `yaml:"encrypted_regex"`
	UnencryptedCommentRegex string     `yaml:"unencrypted_comment_regex"`
	EncryptedCommentRegex   string     `yaml:"encrypted_comment_regex"`
	MACOnlyEncrypted        bool       `yaml:"mac_only_encrypted"`
	Version                 string     `yaml:"version"`
}

// DecryptYAML decrypts a SOPS-encrypted YAML/JSON document with the given age
// identities, verifies its MAC and returns the plaintext YAML.
func DecryptYAML(in []byte, ids []age.Identity) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("top level is not a mapping")
	}
	root := doc.Content[0]
	var md metadata
	idx := -1
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "sops" {
			if err := root.Content[i+1].Decode(&md); err != nil {
				return nil, fmt.Errorf("metadata: %w", err)
			}
			idx = i
		}
	}
	if idx < 0 {
		return nil, errors.New("no sops metadata")
	}
	root.Content = append(root.Content[:idx], root.Content[idx+2:]...)

	key, err := dataKey(md, ids)
	if err != nil {
		return nil, err
	}
	d := &decryptor{md: md, key: key, h: sha512.New()}
	if md.MACOnlyEncrypted {
		d.h.Write(macOnlyEncryptedInit)
	}
	if err := d.walk(root, nil, nil); err != nil {
		return nil, err
	}
	lm, err := time.Parse(time.RFC3339, md.LastModified)
	if err != nil {
		return nil, fmt.Errorf("lastmodified: %w", err)
	}
	stored, _, err := d.decryptValue(md.MAC, lm.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("decrypt mac: %w", err)
	}
	if got := fmt.Sprintf("%X", d.h.Sum(nil)); got != stored {
		return nil, fmt.Errorf("MAC mismatch: stored %q, computed %q", stored, got)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4) // sops' default YAML indent
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

func dataKey(md metadata, ids []age.Identity) ([]byte, error) {
	groups := md.KeyGroups
	if len(groups) == 0 {
		groups = []keyGroup{{Age: md.Age}}
	}
	if len(groups) > 1 && md.ShamirThreshold != 1 {
		return nil, errors.New("shamir key groups not supported")
	}
	var errs []error
	for _, g := range groups {
		for _, k := range g.Age {
			r, err := age.Decrypt(armor.NewReader(strings.NewReader(k.Enc)), ids...)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", k.Recipient, err))
				continue
			}
			b, err := io.ReadAll(r)
			if err != nil {
				return nil, err
			}
			if len(b) != 32 {
				return nil, fmt.Errorf("data key has %d bytes", len(b))
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("no age recipient could be decrypted: %w", errors.Join(errs...))
}

type decryptor struct {
	md  metadata
	key []byte
	h   hash.Hash
}

func (d *decryptor) decryptValue(s, aad string) (plain, typ string, err error) {
	if s == "" {
		return "", "str", nil
	}
	m := encRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", errors.New("value does not match sops format")
	}
	var parts [3][]byte
	for i := range parts {
		if parts[i], err = base64.StdEncoding.DecodeString(m[i+1]); err != nil {
			return "", "", err
		}
	}
	block, err := aes.NewCipher(d.key)
	if err != nil {
		return "", "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(parts[1]))
	if err != nil {
		return "", "", err
	}
	out, err := gcm.Open(nil, parts[1], append(parts[0], parts[2]...), []byte(aad))
	if err != nil {
		return "", "", fmt.Errorf("AES-GCM: %w", err)
	}
	return string(out), m[4], nil
}

// shouldBeEncrypted ports sops.Tree.shouldBeEncrypted.
func (d *decryptor) shouldBeEncrypted(path []string, comments [][]string, isComment bool) bool {
	md := d.md
	enc := true
	if md.UnencryptedSuffix != "" {
		for _, p := range path {
			if strings.HasSuffix(p, md.UnencryptedSuffix) {
				enc = false
				break
			}
		}
	}
	if md.EncryptedSuffix != "" {
		enc = false
		for _, p := range path {
			if strings.HasSuffix(p, md.EncryptedSuffix) {
				enc = true
				break
			}
		}
	}
	if md.UnencryptedRegex != "" {
		for _, p := range path {
			if ok, _ := regexp.MatchString(md.UnencryptedRegex, p); ok {
				enc = false
				break
			}
		}
	}
	if md.EncryptedRegex != "" {
		enc = false
		for _, p := range path {
			if ok, _ := regexp.MatchString(md.EncryptedRegex, p); ok {
				enc = true
				break
			}
		}
	}
	if md.UnencryptedCommentRegex != "" {
	outerU:
		for _, cs := range comments {
			for _, c := range cs {
				if ok, _ := regexp.MatchString(md.UnencryptedCommentRegex, c); ok {
					enc = false
					break outerU
				}
			}
		}
	}
	if md.EncryptedCommentRegex != "" {
		enc = false
		last := len(comments) - 1
	outerE:
		for i, cs := range comments {
			for j, c := range cs {
				if isComment && i == last && j == len(comments[last])-1 {
					continue
				}
				if ok, _ := regexp.MatchString(md.EncryptedCommentRegex, c); ok {
					enc = true
					break outerE
				}
			}
		}
	}
	return enc
}

// walk visits the tree in the order sops' walkBranch/walkSlice do. Comment
// handling is simplified: head comments are tracked for the comment regexes
// and decrypted when they hold an ENC[] value, but are not part of the MAC
// (sops skips Comment values when hashing).
func (d *decryptor) walk(n *yaml.Node, path []string, comments [][]string) error {
	switch n.Kind {
	case yaml.MappingNode:
		comments = append(comments, nil)
		for i := 0; i < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.HeadComment != "" {
				for _, line := range strings.Split(k.HeadComment, "\n") {
					c := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
					comments[len(comments)-1] = append(comments[len(comments)-1], c)
				}
				d.decryptComment(k, path, comments)
			}
			if err := d.walk(v, append(path[:len(path):len(path)], k.Value), comments); err != nil {
				return err
			}
			comments[len(comments)-1] = nil
		}
	case yaml.SequenceNode:
		comments = append(comments, nil)
		for _, v := range n.Content {
			if err := d.walk(v, path, comments); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		return errors.New("aliases not supported")
	case yaml.ScalarNode:
		return d.leaf(n, path, comments)
	}
	return nil
}

func (d *decryptor) decryptComment(k *yaml.Node, path []string, comments [][]string) {
	if !d.shouldBeEncrypted(path, comments, true) {
		return
	}
	lines := strings.Split(k.HeadComment, "\n")
	for i, line := range lines {
		c := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		if p, _, err := d.decryptValue(c, strings.Join(path, ":")+":"); err == nil {
			lines[i] = "#" + p
		}
	}
	k.HeadComment = strings.Join(lines, "\n")
}

func (d *decryptor) leaf(n *yaml.Node, path []string, comments [][]string) error {
	if n.Tag == "!!null" {
		return nil // sops does not walk nil values
	}
	enc := d.shouldBeEncrypted(path, comments, false)
	var macBytes string
	if enc {
		if n.Tag != "!!str" {
			return fmt.Errorf("%s: expected encrypted string, got %s", strings.Join(path, "."), n.Tag)
		}
		plain, typ, err := d.decryptValue(n.Value, strings.Join(path, ":")+":")
		if err != nil {
			return fmt.Errorf("%s: %w", strings.Join(path, "."), err)
		}
		n.Style = 0
		switch typ {
		case "str", "bytes":
			n.Tag, n.Value, macBytes = "!!str", plain, plain
		case "int":
			i, err := strconv.Atoi(plain)
			if err != nil {
				return err
			}
			n.Tag, n.Value, macBytes = "!!int", strconv.Itoa(i), strconv.Itoa(i)
		case "float":
			f, err := strconv.ParseFloat(plain, 64)
			if err != nil {
				return err
			}
			s := strconv.FormatFloat(f, 'f', -1, 64)
			n.Tag, n.Value, macBytes = "!!float", s, s
		case "bool":
			b, err := strconv.ParseBool(plain)
			if err != nil {
				return err
			}
			n.Tag, n.Value = "!!bool", strconv.FormatBool(b)
			macBytes = map[bool]string{true: "True", false: "False"}[b]
		default:
			return fmt.Errorf("%s: unsupported type %q", strings.Join(path, "."), typ)
		}
	} else {
		var v any
		if err := n.Decode(&v); err != nil {
			return err
		}
		switch v := v.(type) {
		case string:
			macBytes = v
		case int:
			macBytes = strconv.Itoa(v)
		case float64:
			macBytes = strconv.FormatFloat(v, 'f', -1, 64)
		case bool:
			macBytes = map[bool]string{true: "True", false: "False"}[v]
		default:
			return fmt.Errorf("%s: unsupported unencrypted value %T", strings.Join(path, "."), v)
		}
	}
	if !d.md.MACOnlyEncrypted || enc {
		d.h.Write([]byte(macBytes))
	}
	return nil
}
