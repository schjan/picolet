package sops

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
)

// CreationRulesFile is SOPS's config file, read only at the Fleet root:
// nested ones apply only from certain working directories.
const CreationRulesFile = ".sops.yaml"

// CreationRules are the creation rules of a `.sops.yaml`. Only creation_rules
// and their age recipients are read; everything else is ignored.
type CreationRules struct {
	rules []rawRule
}

type rawRule struct {
	PathRegex string     `yaml:"path_regex"`
	Age       ageKeys    `yaml:"age"`
	KeyGroups []rawGroup `yaml:"key_groups"`
}

// ageKeys is a rule's top-level age recipients, written as one
// comma-separated string or a list of them; each entry is split as sops does.
type ageKeys []string

// UnmarshalYAML accepts a string or a list of strings, as sops does.
func (a *ageKeys) UnmarshalYAML(node *yaml.Node) error {
	entries := []*yaml.Node{node}
	if node.Kind == yaml.SequenceNode {
		entries = node.Content
	}
	var keys []string
	for _, e := range entries {
		if e.Kind != yaml.ScalarNode || e.ShortTag() != "!!str" {
			return errors.New("age: expected a string or a list of strings")
		}
		keys = append(keys, splitRecipients(e.Value)...)
	}
	*a = keys
	return nil
}

type rawGroup struct {
	Merge []rawGroup `yaml:"merge"`
	Age   []string   `yaml:"age"`
}

// CreationRule is the rule of a `.sops.yaml` that matched a file.
type CreationRule struct {
	// Index is the rule's position in creation_rules, from 0.
	Index int
	// PathRegex is the rule's path_regex as written; empty matches every file.
	PathRegex string
	raw       rawRule
}

// ParseCreationRules parses a `.sops.yaml`. Rules are not compiled here: as
// upstream, a path_regex is compiled only when matching reaches it.
func ParseCreationRules(data []byte) (*CreationRules, error) {
	var conf struct {
		CreationRules []rawRule `yaml:"creation_rules"`
	}
	if err := yaml.Unmarshal(data, &conf); err != nil {
		return nil, fmt.Errorf("parsing creation rules: %w", err)
	}
	return &CreationRules{rules: conf.CreationRules}, nil
}

// ReadCreationRules reads the Fleet-root `.sops.yaml` from fsys. Without one
// it returns rules that match no file. The file must be a regular file: a
// symlink is an error and never followed, and an fsys that cannot Lstat
// fails closed.
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
	rules, err := ParseCreationRules(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", CreationRulesFile, err)
	}
	return rules, nil
}

// Match returns the rule `sops` applies to path, relative to the Fleet root:
// rules are tried in order and the first whose path_regex matches wins; an
// empty path_regex matches every file. ok is false when no rule matches. An
// invalid path_regex reached before a match is an error.
func (c *CreationRules) Match(path string) (rule *CreationRule, ok bool, err error) {
	for i, r := range c.rules {
		if r.PathRegex != "" {
			re, err := regexp.Compile(r.PathRegex)
			if err != nil {
				return nil, false, fmt.Errorf("%s: creation rule %d: path_regex %q: %w", CreationRulesFile, i+1, r.PathRegex, err)
			}
			if !re.MatchString(path) {
				continue
			}
		}
		return &CreationRule{Index: i, PathRegex: r.PathRegex, raw: r}, true, nil
	}
	return nil, false, nil
}

func (r *CreationRule) label() string {
	if r.PathRegex == "" {
		return fmt.Sprintf("%d (empty path_regex)", r.Index+1)
	}
	return "`" + r.PathRegex + "`"
}

// ageGroups returns the rule's age recipient labels per key group: its
// key_groups (merges flattened), or one group of its top-level age keys.
func (r *CreationRule) ageGroups() [][]string {
	if len(r.raw.KeyGroups) > 0 {
		groups := make([][]string, 0, len(r.raw.KeyGroups))
		for _, g := range r.raw.KeyGroups {
			groups = append(groups, groupLabels(g))
		}
		return groups
	}
	return [][]string{labels(r.raw.Age)}
}

func groupLabels(g rawGroup) []string {
	var keys []string
	for _, m := range g.Merge {
		keys = append(keys, groupLabels(m)...)
	}
	for _, a := range g.Age {
		keys = append(keys, labels(splitRecipients(a))...)
	}
	return keys
}

// splitRecipients splits a comma-separated recipient list as sops does.
func splitRecipients(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// labels returns each recipient's comparison label: the canonical form when
// it parses, the (redacted) label as written otherwise.
func labels(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if r, err := ParseRecipient(k); err == nil {
			out = append(out, r.String())
			continue
		}
		out = append(out, recipientLabel(k))
	}
	return out
}

// fileGroups returns f's age recipient labels per key group, in the same
// vocabulary as labels.
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

// CheckRecipients compares the age Recipients of f, the SOPS file at path
// (Fleet-relative), with the rule's, group by group: the same number of
// groups and the same set in each, order ignored. shamir_threshold, the
// encryption rules and non-age keys are ignored. The error lists per group
// the Recipients in the file but not in the rule (+) and in the rule but not
// in the file (−), and the `sops updatekeys` fix.
func (r *CreationRule) CheckRecipients(path string, f *File) error {
	want := r.ageGroups()
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
	return fmt.Errorf("`%s` recipients differ from `%s` rule %s: %s; run `sops updatekeys %s`",
		path, CreationRulesFile, r.label(), strings.Join(parts, "; "), path)
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
