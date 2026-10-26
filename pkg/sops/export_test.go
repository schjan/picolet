package sops

// PointerSegments exposes a Pointer's unescaped segments to the external
// tests; nil for the whole document.
func PointerSegments(p Pointer) []string {
	if len(p.segments) == 0 {
		return nil
	}
	return p.segments
}

// NoMatchingRule is upstream's no-matching-rule error text, which
// CreationRules.Check detects.
const NoMatchingRule = noMatchingRule

// RulesDir exposes the private copy's directory.
func RulesDir(c *CreationRules) string { return c.dir }
