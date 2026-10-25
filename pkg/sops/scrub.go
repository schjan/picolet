package sops

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	gsops "github.com/getsops/sops/v3"
)

// MinScrubLen is the shortest decrypted value Scrub removes. Shorter values
// ("app", "1") would blank ordinary words of an error message.
const MinScrubLen = 4

// Redacted replaces a decrypted value Scrub finds in a text.
const Redacted = "<redacted>"

// yamlTruncateOver and yamlTruncateKeep mirror how the YAML library
// (go.yaml.in/yaml) quotes a scalar in a type error: a value longer than
// yamlTruncateOver bytes appears as its first yamlTruncateKeep bytes and
// "...". Validator errors on manifests come from it.
const (
	yamlTruncateOver = 10
	yamlTruncateKeep = 7
)

// Scrub returns text with every decrypted leaf value of MinScrubLen or more
// characters that this Reader produced replaced by Redacted: the values of
// every leaf of every file it decrypted, not only those a ref named. It
// catches the spellings errors embed a value in: as is, Go-quoted (%q), each
// line of a multi-line value, and the YAML library's truncated form. A nil
// Reader, or one that decrypted nothing, returns text unchanged.
//
// Callers scrub every error of a resolve that used this Reader before the
// error reaches a log, an event or a status report: template helpers and
// validators may embed a rendered value in their messages.
func (r *Reader) Scrub(text string) string {
	return r.Scrubber().Scrub(text)
}

// Scrubber returns a Scrubber for the values this Reader has decrypted so
// far; it does not see files the Reader decrypts later. It holds only the
// strings to replace, not the decrypted files. A nil Reader returns the
// zero Scrubber, which changes nothing.
func (r *Reader) Scrubber() Scrubber {
	if r == nil {
		return Scrubber{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.needles == nil {
		r.needles = scrubNeedles(r.values)
	}
	return Scrubber{needles: r.needles}
}

// Scrubber removes a Reader's decrypted values from text (Reader.Scrub).
type Scrubber struct {
	needles []string // longest first; shared with the Reader's cache, never changed
}

// Scrub returns text with the Scrubber's values replaced by Redacted.
func (s Scrubber) Scrub(text string) string {
	for _, n := range s.needles {
		text = strings.ReplaceAll(text, n, Redacted)
	}
	return text
}

// scrubNeedles returns the strings to replace for values, longest first, so
// a value is replaced whole before a shorter value inside it.
func scrubNeedles(values map[string]struct{}) []string {
	set := map[string]struct{}{}
	add := func(s string) {
		if len(s) >= MinScrubLen {
			set[s] = struct{}{}
		}
	}
	for v := range values {
		if len(v) < MinScrubLen {
			continue
		}
		add(v)
		quoted := strconv.Quote(v)
		add(quoted[1 : len(quoted)-1])
		if strings.Contains(v, "\n") {
			for line := range strings.SplitSeq(v, "\n") {
				add(strings.TrimSpace(line))
			}
		}
		if len(v) > yamlTruncateOver {
			add(v[:yamlTruncateKeep] + "...")
		}
	}
	needles := make([]string, 0, len(set))
	for s := range set {
		needles = append(needles, s)
	}
	slices.SortFunc(needles, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	return needles
}

// collectLeaves adds the rendered value of every leaf of p to into: strings
// as they are, other scalars as `sops decrypt --extract` prints them.
func (p *plainFile) collectLeaves(into map[string]struct{}) {
	for _, b := range p.branches {
		p.collect(b, into)
	}
}

func (p *plainFile) collect(v any, into map[string]struct{}) {
	switch v := v.(type) {
	case gsops.TreeBranch:
		for _, item := range v {
			p.collect(item.Value, into)
		}
	case []any:
		for _, e := range v {
			p.collect(e, into)
		}
	case nil:
	case string:
		into[v] = struct{}{}
	default:
		if out, err := p.store.EmitValue(v); err == nil {
			into[strings.TrimSpace(string(out))] = struct{}{}
		}
	}
}
