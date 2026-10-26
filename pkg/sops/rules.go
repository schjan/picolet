package sops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/config"
)

// CreationRulesFile is SOPS's config file, read only at the Fleet root:
// nested ones apply only from certain working directories.
const CreationRulesFile = ".sops.yaml"

// noMatchingRule is upstream's error text when no creation rule matches a
// file (pinned by a test, so a SOPS upgrade that changes it fails loudly).
const noMatchingRule = "error loading config: no matching creation rules found"

// ErrInvalidCreationRule is returned (wrapped) by CreationRules.Check when
// upstream cannot load the creation rule for a file.
var ErrInvalidCreationRule = errors.New("invalid")

// CreationRules is the Fleet-root `.sops.yaml`, loaded by upstream's config
// loader: a private copy in a temp dir, removed by Close.
type CreationRules struct {
	// dir holds the copy as CreationRulesFile; empty when the Fleet has none.
	dir string
}

// ReadCreationRules copies the Fleet-root `.sops.yaml` from fsys into a
// private temp dir. Without one it returns rules that check no file. The
// file must be a regular file of at most MaxFileSize: a symlink is an error
// and never followed, and an fsys that cannot Lstat fails closed. The caller
// must Close the result.
func ReadCreationRules(fsys fs.FS) (*CreationRules, error) {
	lfs, ok := fsys.(fs.ReadLinkFS)
	if !ok {
		return nil, fmt.Errorf("%s: cannot tell whether it is a symlink (the Fleet FS has no Lstat)", CreationRulesFile)
	}
	info, err := lfs.Lstat(CreationRulesFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &CreationRules{}, nil
	case err != nil:
		return nil, fmt.Errorf("%s: %w", CreationRulesFile, err)
	case info.Mode()&fs.ModeSymlink != 0:
		return nil, fmt.Errorf("%s: is a symlink; commit the creation rules as a regular file", CreationRulesFile)
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("%s: not a regular file", CreationRulesFile)
	}
	data, err := ReadFile(fsys, CreationRulesFile)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "picolet-sops-rules-")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", CreationRulesFile, err)
	}
	// Upstream trims the config's absolute dir from the file path; a
	// relative TMPDIR would leave the prefix on and match nothing.
	abs, err := filepath.Abs(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("%s: %w", CreationRulesFile, err)
	}
	dir = abs
	if err := os.WriteFile(filepath.Join(dir, CreationRulesFile), data, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("%s: %w", CreationRulesFile, err)
	}
	return &CreationRules{dir: dir}, nil
}

// Close removes the private copy.
func (c *CreationRules) Close() error {
	if c.dir == "" {
		return nil
	}
	return os.RemoveAll(c.dir)
}

// Check compares the age Recipients of f, the SOPS file at path
// (Fleet-relative), with those of the creation rule upstream picks for it,
// group by group: the same number of groups and the same set in each, order
// ignored; a file without key groups is one group. shamir_threshold, the
// encryption rules and non-age keys are ignored. A Fleet without
// creation_rules, or no rule matching path, passes. A rule upstream cannot
// load is an error. A drift error lists per group the Recipients in the file
// but not in the rule (+) and in the rule but not in the file (−), and the
// `sops updatekeys` fix.
func (c *CreationRules) Check(path string, f *File) error {
	if c.dir == "" {
		return nil
	}
	// The file need not exist: upstream matches the path with the config's
	// dir trimmed, so path_regex sees path as written, relative to the root.
	conf, err := config.LoadCreationRuleForFile(filepath.Join(c.dir, CreationRulesFile),
		c.dir+string(filepath.Separator)+path, nil)
	switch {
	case err != nil && err.Error() == noMatchingRule:
		return nil
	case err != nil:
		return fmt.Errorf("`%s` rule for `%s` is %w: %s", CreationRulesFile, path, ErrInvalidCreationRule, ruleErrorText(err))
	case conf == nil:
		return nil
	}
	return checkRuleRecipients(path, f, ruleGroups(conf))
}

// ruleErrorText shortens upstream's error for an invalid rule. The config
// holds public data only, but a secret key pasted in as a recipient would be
// quoted, so such an error is replaced by a fixed message.
func ruleErrorText(err error) string {
	const maxLen = 200
	text := err.Error()
	if strings.Contains(strings.ToUpper(text), ageIdentityPrefix) {
		return "an age identity (secret key) is not an age recipient; use its public key"
	}
	if r := []rune(text); len(r) > maxLen {
		text = string(r[:maxLen]) + "…"
	}
	return text
}

// ruleGroups returns the age recipient labels per key group of conf.
func ruleGroups(conf *config.Config) [][]string {
	groups := make([][]string, 0, len(conf.KeyGroups))
	for _, g := range conf.KeyGroups {
		var keys []string
		for _, k := range g {
			if ak, ok := k.(*age.MasterKey); ok {
				keys = append(keys, label(ak.Recipient))
			}
		}
		groups = append(groups, keys)
	}
	return groups
}

// label returns a recipient's comparison label: the canonical form when it
// parses, the (redacted) label as written otherwise (SSH, plugin), as the
// file's are.
func label(key string) string {
	if r, err := ParseRecipient(key); err == nil {
		return r.String()
	}
	return recipientLabel(key)
}

// fileGroups returns f's age recipient labels per key group, in the same
// vocabulary as label.
func fileGroups(f *File) [][]string {
	groups := make([][]string, 0, len(f.KeyGroups))
	for _, g := range f.KeyGroups {
		groups = append(groups, append(RecipientStrings(g.Age), g.UnsupportedAge...))
	}
	if len(groups) == 0 {
		groups = append(groups, nil)
	}
	return groups
}

// checkRuleRecipients compares f's age recipient groups with want; see Check.
func checkRuleRecipients(path string, f *File, want [][]string) error {
	got := fileGroups(f)
	var parts []string
	if len(got) != len(want) {
		parts = append(parts, fmt.Sprintf("file has %d key groups, rule has %d", len(got), len(want)))
	}
	multi := max(len(got), len(want)) > 1
	for i := range max(len(got), len(want)) {
		var g, w []string
		if i < len(got) {
			g = got[i]
		}
		if i < len(want) {
			w = want[i]
		}
		d := setDiff(g, w)
		if d == "" {
			continue
		}
		if multi {
			d = fmt.Sprintf("key group %d: %s", i+1, d)
		}
		parts = append(parts, d)
	}
	if len(parts) == 0 {
		return nil
	}
	return fmt.Errorf("`%s` recipients differ from the `%s` creation rule matching it: %s; run `sops updatekeys %s`",
		path, CreationRulesFile, strings.Join(parts, "; "), path)
}

// setDiff renders the labels only in got as "+x" (in got's order) and those
// only in want as "−x" (in want's order); empty when the sets are equal.
func setDiff(got, want []string) string {
	type entry struct {
		sign  string
		label string
	}
	var out []string
	seen := map[entry]bool{}
	add := func(e entry) {
		if !seen[e] {
			seen[e] = true
			out = append(out, e.sign+e.label)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			add(entry{"+", g})
		}
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			add(entry{"−", w})
		}
	}
	return strings.Join(out, " ")
}
