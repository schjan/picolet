package sops

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strconv"
	"time"

	gsops "github.com/getsops/sops/v3"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/stores/dotenv"
	"github.com/getsops/sops/v3/stores/ini"
	"github.com/getsops/sops/v3/stores/json"
	"github.com/getsops/sops/v3/stores/yaml"
)

// MaxFileSize is the largest SOPS file picolet reads (1 MiB), so a fork PR
// can't feed CI an unbounded file.
const MaxFileSize = 1 << 20

var (
	// ErrFileTooLarge is returned (wrapped) for a SOPS file over MaxFileSize.
	ErrFileTooLarge = errors.New("SOPS file too large")
	// ErrNotSOPSFile is returned (wrapped) when a lookup needs SOPS data but
	// the file carries no valid SOPS metadata.
	ErrNotSOPSFile = errors.New("not a SOPS file")
)

// Format is the SOPS store a file is read with.
type Format string

const (
	FormatYAML   Format = "yaml"
	FormatJSON   Format = "json"
	FormatDotenv Format = "dotenv"
	FormatINI    Format = "ini"
	FormatBinary Format = "binary"
)

// File is the keyless view of a SOPS file: its metadata and the shape of its
// tree. Nothing in it needs, or reveals, a key or a plaintext secret: encrypted
// leaves are reported by type tag only.
type File struct {
	// Format is the store chosen from the path's extension, as `sops` does:
	// yaml, json, dotenv, ini or binary.
	Format Format `json:"format"`
	// IsSOPS is true when the file parses with SOPS metadata carrying
	// sops.version and sops.mac (the rule of `sops filestatus`).
	IsSOPS bool `json:"is_sops"`
	// NotSOPSReason says why IsSOPS is false. It never quotes file content.
	NotSOPSReason string `json:"not_sops_reason,omitempty"`

	Version string `json:"version,omitempty"`
	// KeyGroups lists the master keys per key group. A file without
	// key_groups has exactly one group.
	KeyGroups []KeyGroup `json:"key_groups,omitempty"`
	// ShamirThreshold is sops.shamir_threshold as stored; 0 means every group
	// is required (see RequiredGroups).
	ShamirThreshold int `json:"shamir_threshold"`

	// The file's encryption rules, as recorded in its metadata. picolet does
	// not apply them; they are reported for diagnostics.
	UnencryptedSuffix       string `json:"unencrypted_suffix,omitempty"`
	EncryptedSuffix         string `json:"encrypted_suffix,omitempty"`
	UnencryptedRegex        string `json:"unencrypted_regex,omitempty"`
	EncryptedRegex          string `json:"encrypted_regex,omitempty"`
	UnencryptedCommentRegex string `json:"unencrypted_comment_regex,omitempty"`
	EncryptedCommentRegex   string `json:"encrypted_comment_regex,omitempty"`
	MACOnlyEncrypted        bool   `json:"mac_only_encrypted,omitempty"`

	tree gsops.TreeBranches
}

// KeyGroup is one SOPS key group.
type KeyGroup struct {
	// Age holds the group's age recipients in canonical form, in file order.
	Age []Recipient `json:"age,omitempty"`
	// UnsupportedAge holds age recipient labels that are not X25519 or hybrid
	// post-quantum keys (SSH, plugin, malformed), as written; an age identity
	// (secret key) is redacted to "AGE-SECRET-KEY-<redacted>".
	UnsupportedAge []string `json:"unsupported_age,omitempty"`
	// Other holds the key type of every non-age master key ("kms", "pgp",
	// "gcp_kms", ...), in file order. picolet never uses them.
	Other []string `json:"other,omitempty"`
}

// RequiredGroups returns how many key groups a reader must open: the Shamir
// threshold, or every group when the threshold is 0.
func (f *File) RequiredGroups() int {
	if f.ShamirThreshold <= 0 || f.ShamirThreshold > len(f.KeyGroups) {
		return len(f.KeyGroups)
	}
	return f.ShamirThreshold
}

// ReadFile reads a SOPS file from fsys, failing with ErrFileTooLarge before
// reading more than MaxFileSize+1 bytes.
func ReadFile(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if err := checkSize(name, data); err != nil {
		return nil, err
	}
	return data, nil
}

func checkSize(name string, data []byte) error {
	if len(data) > MaxFileSize {
		return fmt.Errorf("%s: %w (over %d bytes)", name, ErrFileTooLarge, MaxFileSize)
	}
	return nil
}

// store is the part of a SOPS store picolet uses: loading an encrypted file
// without any key, and emitting decrypted data as `sops decrypt` does.
type store interface {
	gsops.EncryptedFileLoader
	gsops.PlainFileEmitter
	gsops.ValueEmitter
}

// storeFor picks the store from the path's extension with upstream's rule,
// configured with the defaults the sops CLI uses without a config file, so
// decrypted output renders as `sops decrypt` prints it.
//
//nolint:ireturn // the store's concrete type depends on the format
func storeFor(path string) (Format, store) {
	c := config.NewStoresConfig()
	switch formats.FormatForPath(path) {
	case formats.Yaml:
		return FormatYAML, yaml.NewStore(&c.YAML)
	case formats.Json:
		return FormatJSON, json.NewStore(&c.JSON)
	case formats.Dotenv:
		return FormatDotenv, dotenv.NewStore(&c.Dotenv)
	case formats.Ini:
		return FormatINI, ini.NewStore(&c.INI)
	default:
		return FormatBinary, json.NewBinaryStore(&c.JSONBinary)
	}
}

// Inspect parses a SOPS file without any key. path selects the format (by
// extension, as `sops` does) and names the file in errors. A file that does
// not parse, or carries no valid SOPS metadata, is reported with IsSOPS false,
// not as an error; only a file over MaxFileSize is an error.
func Inspect(path string, data []byte) (*File, error) {
	if err := checkSize(path, data); err != nil {
		return nil, err
	}
	format, st := storeFor(path)
	f := &File{Format: format}
	tree, err := st.LoadEncryptedFile(data)
	switch {
	case errors.Is(err, gsops.MetadataNotFound):
		f.NotSOPSReason = "no sops metadata"
		return f, nil
	case err != nil:
		// Upstream parse errors can quote file content: never pass them on.
		f.NotSOPSReason = "cannot be parsed as " + string(format) + " with sops metadata"
		return f, nil
	case tree.Metadata.Version == "":
		f.NotSOPSReason = "sops.version missing"
		return f, nil
	case tree.Metadata.MessageAuthenticationCode == "":
		f.NotSOPSReason = "sops.mac missing"
		return f, nil
	}
	f.IsSOPS = true
	f.fillMetadata(tree.Metadata)
	f.tree = tree.Branches
	return f, nil
}

func (f *File) fillMetadata(md gsops.Metadata) {
	f.Version = md.Version
	f.ShamirThreshold = md.ShamirThreshold
	f.UnencryptedSuffix = md.UnencryptedSuffix
	f.EncryptedSuffix = md.EncryptedSuffix
	f.UnencryptedRegex = md.UnencryptedRegex
	f.EncryptedRegex = md.EncryptedRegex
	f.UnencryptedCommentRegex = md.UnencryptedCommentRegex
	f.EncryptedCommentRegex = md.EncryptedCommentRegex
	f.MACOnlyEncrypted = md.MACOnlyEncrypted
	for _, group := range md.KeyGroups {
		var g KeyGroup
		for _, key := range group {
			ak, ok := key.(*sopsage.MasterKey)
			if !ok {
				g.Other = append(g.Other, key.TypeToIdentifier())
				continue
			}
			r, err := ParseRecipient(ak.Recipient)
			if err != nil {
				g.UnsupportedAge = append(g.UnsupportedAge, recipientLabel(ak.Recipient))
				continue
			}
			g.Age = append(g.Age, r)
		}
		f.KeyGroups = append(f.KeyGroups, g)
	}
}

// NodeKind is the kind of node a pointer ends at.
type NodeKind string

const (
	NodeLeaf NodeKind = "leaf"
	NodeMap  NodeKind = "map"
	NodeList NodeKind = "list"
)

// Node describes what a pointer ends at, without its value.
type Node struct {
	Kind NodeKind `json:"kind"`
	// Encrypted is true for a leaf in SOPS's ENC[AES256_GCM,…] form.
	Encrypted bool `json:"encrypted,omitempty"`
	// Type is a leaf's type: the type tag of an encrypted leaf (str, int,
	// float, bool, time, bytes, comment), or the type of a plaintext leaf in
	// the same vocabulary plus "null". Empty for maps and lists.
	Type string `json:"type,omitempty"`
}

// IsLeaf reports whether the node is a leaf.
func (n Node) IsLeaf() bool { return n.Kind == NodeLeaf }

// Lookup resolves p in the file's data (the first document of a multi-document
// file) and describes the node it ends at. A path that names no node is
// ErrBadPointer; a file that is not a SOPS file is ErrNotSOPSFile.
func (f *File) Lookup(p Pointer) (Node, error) {
	if !f.IsSOPS {
		return Node{}, fmt.Errorf("%w: %s", ErrNotSOPSFile, f.NotSOPSReason)
	}
	v, err := resolvePointer(f.tree, p)
	if err != nil {
		return Node{}, err
	}
	return describe(v), nil
}

// encRe matches SOPS's encrypted value format (aes/cipher.go).
var encRe = regexp.MustCompile(`^ENC\[AES256_GCM,data:(.+),iv:(.+),tag:(.+),type:(.+)\]`)

func describe(v any) Node {
	switch v := v.(type) {
	case gsops.TreeBranch:
		return Node{Kind: NodeMap}
	case []any:
		return Node{Kind: NodeList}
	case string:
		if m := encRe.FindStringSubmatch(v); m != nil {
			return Node{Kind: NodeLeaf, Encrypted: true, Type: m[4]}
		}
	}
	return Node{Kind: NodeLeaf, Type: plainType(v)}
}

// plainType names a plaintext leaf's type in the type-tag vocabulary.
func plainType(v any) string {
	switch v.(type) {
	case string:
		return "str"
	case []byte:
		return "bytes"
	case int, int64:
		return "int"
	case float64:
		return "float"
	case bool:
		return "bool"
	case time.Time:
		return "time"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("unknown(%T)", v)
	}
}

// resolvePointer walks p through a SOPS tree (encrypted or decrypted). Each
// segment is a list index on a list and a map key otherwise. Comment items
// are not data: they are skipped by key lookups and list indices alike.
func resolvePointer(branches gsops.TreeBranches, p Pointer) (any, error) {
	if len(branches) == 0 {
		return nil, fmt.Errorf("%w %q: the file holds no data", ErrBadPointer, p.String())
	}
	var cur any = branches[0]
	for i, seg := range p.segments {
		at := Pointer{segments: p.segments[:i]}.String()
		switch node := cur.(type) {
		case gsops.TreeBranch:
			v, ok := lookupKey(node, seg)
			if !ok {
				return nil, fmt.Errorf("%w %q: key %q not found at %q", ErrBadPointer, p.String(), seg, rootLabel(at))
			}
			cur = v
		case []any:
			v, err := lookupIndex(node, seg)
			if err != nil {
				return nil, fmt.Errorf("%w %q: at %q: %w", ErrBadPointer, p.String(), rootLabel(at), err)
			}
			cur = v
		default:
			return nil, fmt.Errorf("%w %q: %q is a leaf and has no %q", ErrBadPointer, p.String(), rootLabel(at), seg)
		}
	}
	return cur, nil
}

func rootLabel(at string) string {
	if at == "" {
		return "/"
	}
	return at
}

func lookupKey(branch gsops.TreeBranch, key string) (any, bool) {
	for _, item := range branch {
		if k, ok := item.Key.(string); ok && k == key {
			return item.Value, true
		}
	}
	return nil, false
}

// indexRe is RFC 6901's array-index: "0" or a number without leading zeros.
var indexRe = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

func lookupIndex(list []any, seg string) (any, error) {
	if !indexRe.MatchString(seg) {
		return nil, fmt.Errorf("%q is not a list index", seg)
	}
	idx, err := strconv.Atoi(seg)
	if err != nil {
		return nil, fmt.Errorf("%q is not a list index", seg)
	}
	n := 0
	for _, item := range list {
		if _, isComment := item.(gsops.Comment); isComment {
			continue
		}
		if n == idx {
			return item, nil
		}
		n++
	}
	return nil, fmt.Errorf("index %d out of range (list has %d items)", idx, n)
}
