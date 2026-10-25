package sops

// PointerSegments exposes a Pointer's unescaped segments to the external
// tests; nil for the whole document.
func PointerSegments(p Pointer) []string {
	if len(p.segments) == 0 {
		return nil
	}
	return p.segments
}
