package sops

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Scheme is the Secret Reference scheme of SOPS-encrypted values.
const Scheme = "sops://"

var (
	// ErrBadPointer is returned (wrapped) for a malformed JSON Pointer or one
	// that names no node in the file.
	ErrBadPointer = errors.New("bad pointer")
	// ErrNotSOPSPath is returned (wrapped) for a path without the `.sops.`
	// infix, and for SOPS's own config files (.sops.yaml, .sops.yml).
	ErrNotSOPSPath = errors.New("not a SOPS path")
	// ErrInvalidSecretName is returned (wrapped) when a direct ref cannot be
	// turned into a Podman secret name.
	ErrInvalidSecretName = errors.New("invalid Podman secret name")
)

// Pointer is an RFC 6901 JSON Pointer into a SOPS file. The zero value (no
// segments) addresses the whole document. On a list a segment is a list index;
// everywhere else it is a map key.
type Pointer struct {
	segments []string
}

// ParsePointer parses an RFC 6901 pointer: "" or a sequence of "/segment",
// with "~1" for "/" and "~0" for "~" inside a segment.
func ParsePointer(s string) (Pointer, error) {
	if s == "" {
		return Pointer{}, nil
	}
	if !strings.HasPrefix(s, "/") {
		return Pointer{}, fmt.Errorf("%w %q: a JSON Pointer starts with '/'", ErrBadPointer, s)
	}
	raw := strings.Split(s[1:], "/")
	segs := make([]string, len(raw))
	for i, seg := range raw {
		unescaped, err := unescapeSegment(seg)
		if err != nil {
			return Pointer{}, fmt.Errorf("%w %q: %w", ErrBadPointer, s, err)
		}
		segs[i] = unescaped
	}
	return Pointer{segments: segs}, nil
}

var (
	// segmentUnescaper decodes RFC 6901 escapes left to right, so "~01" is "~1".
	segmentUnescaper = strings.NewReplacer("~1", "/", "~0", "~")
	// segmentEscapeChecker drops every valid escape; a '~' left over is invalid.
	segmentEscapeChecker = strings.NewReplacer("~1", "", "~0", "")
)

func unescapeSegment(seg string) (string, error) {
	if strings.Contains(segmentEscapeChecker.Replace(seg), "~") {
		return "", errors.New("'~' must be followed by '0' (for '~') or '1' (for '/')")
	}
	return segmentUnescaper.Replace(seg), nil
}

// String returns the pointer in RFC 6901 syntax.
func (p Pointer) String() string {
	var b strings.Builder
	for _, seg := range p.segments {
		b.WriteByte('/')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(seg, "~", "~0"), "/", "~1"))
	}
	return b.String()
}

// sopsConfigNames are SOPS's own config files, never SOPS paths.
var sopsConfigNames = map[string]bool{".sops.yaml": true, ".sops.yml": true}

// IsConfigPath reports whether p names one of SOPS's own config files
// (`.sops.yaml`, `.sops.yml`), by base name.
func IsConfigPath(p string) bool {
	return sopsConfigNames[path.Base(p)]
}

// IsSOPSPath reports whether p names a SOPS file: its base name carries
// `.sops.` somewhere before its final extension, followed by at least one
// character (`db.sops.yaml`, `db.sops.prod.yaml`). The format is then chosen
// from the final extension, so `db.sops.yaml.bak` is a SOPS path read as
// binary. SOPS's config files `.sops.yaml` and `.sops.yml` are never SOPS
// paths.
func IsSOPSPath(p string) bool {
	if IsConfigPath(p) {
		return false
	}
	_, ext, ok := strings.Cut(path.Base(p), ".sops.")
	return ok && ext != ""
}

// IsRef reports whether s is a sops:// Secret Reference, by scheme;
// ParseRef checks the rest.
func IsRef(s string) bool { return strings.HasPrefix(s, Scheme) }

// Ref is a parsed sops:// Secret Reference:
// sops://<fleet-relative path>[#<JSON Pointer>]. Without a fragment it names
// the whole file.
type Ref struct {
	raw string
	// Path is the Fleet-relative path of the SOPS file, as written. Its
	// validation against the Fleet tree is the caller's.
	Path string
	// Pointer addresses the value; the root pointer for a whole-file ref.
	Pointer Pointer
	whole   bool
}

// ParseRef parses a sops:// ref. The path must be a SOPS path; a fragment, if
// present, must be a non-empty JSON Pointer.
func ParseRef(s string) (Ref, error) {
	rest, ok := strings.CutPrefix(s, Scheme)
	if !ok {
		return Ref{}, fmt.Errorf("%s: not a sops:// reference", s)
	}
	p, frag, hasFrag := strings.Cut(rest, "#")
	if p == "" {
		return Ref{}, fmt.Errorf("%s: missing file path", s)
	}
	if !IsSOPSPath(p) {
		return Ref{}, fmt.Errorf("%s: %w: %q needs `.sops.` before its extension (e.g. db.sops.yaml)", s, ErrNotSOPSPath, p)
	}
	r := Ref{raw: s, Path: p, whole: !hasFrag}
	if !hasFrag {
		return r, nil
	}
	if frag == "" {
		return Ref{}, fmt.Errorf("%s: %w: empty fragment; omit '#' for the whole file", s, ErrBadPointer)
	}
	ptr, err := ParsePointer(frag)
	if err != nil {
		return Ref{}, fmt.Errorf("%s: %w", s, err)
	}
	r.Pointer = ptr
	return r, nil
}

// String returns the ref as written.
func (r Ref) String() string { return r.raw }

// WholeFile reports whether the ref has no fragment and names the whole file.
func (r Ref) WholeFile() bool { return r.whole }

// maxSecretNameLen is Podman's limit on secret names.
const maxSecretNameLen = 253

// SecretName returns the Podman secret name of a direct secrets: ref: the
// pointer's segments joined by "_", or, for a whole-file ref, the file's base
// name before `.sops.`. Characters are never rewritten: a segment outside
// [A-Za-z0-9_.-], an empty segment, a name not starting with a letter or
// digit, or a name over 253 characters is an error.
func (r Ref) SecretName() (string, error) {
	segs := r.Pointer.segments
	if r.whole {
		stem, _, _ := strings.Cut(path.Base(r.Path), ".sops.")
		segs = []string{stem}
	}
	for _, seg := range segs {
		if !validNameSegment(seg) {
			return "", r.invalidName(fmt.Sprintf("segment %q can't appear in a Podman secret name (allowed: letters, digits, '.', '_', '-')", seg))
		}
	}
	if len(segs) == 0 {
		// A whole-file ref always has a stem segment; a fragment ref has at
		// least one segment (ParseRef rejects an empty fragment).
		return "", r.invalidName("no segment to name the secret after")
	}
	// Every segment is non-empty (validNameSegment), so name[0] exists.
	name := strings.Join(segs, "_")
	if !isAlnum(name[0]) {
		return "", r.invalidName(fmt.Sprintf("secret name %q must start with a letter or digit (segment %q)", name, segs[0]))
	}
	if len(name) > maxSecretNameLen {
		return "", r.invalidName(fmt.Sprintf("secret name %q is longer than %d characters", name, maxSecretNameLen))
	}
	return name, nil
}

func (r Ref) invalidName(reason string) error {
	return fmt.Errorf("%s: %w: %s; deliver the value through a secrets/<name>.tmpl file containing {{ readSopsSecret %q }}",
		r.raw, ErrInvalidSecretName, reason, r.raw)
}

func validNameSegment(seg string) bool {
	if seg == "" {
		return false
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if !isAlnum(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
