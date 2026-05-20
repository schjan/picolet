package config

import (
	"errors"
	"path"
	"strings"
)

// ErrNotCleanRelPath is returned by ValidateRelPath when the input is not a
// clean, non-escaping relative path. Callers should wrap with their own
// context (e.g. "manifests[0]: %q %w") so the existing error wording is
// preserved.
var ErrNotCleanRelPath = errors.New("must be a clean relative path")

// ValidateRelPath returns the cleaned form of a relative path used to address
// a file inside a bundle category directory (e.g. manifests/, files/). It
// rejects empty strings, absolute paths, traversal segments, double slashes,
// trailing slashes, and any input that does not equal path.Clean(input).
// On success the cleaned path is returned; on failure ErrNotCleanRelPath
// is returned with no embedded path so callers can preserve their existing
// error format.
func ValidateRelPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	cleaned := path.Clean(trimmed)
	if trimmed == "" || trimmed != cleaned ||
		cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, "/") ||
		strings.HasPrefix(cleaned, "../") {
		return "", ErrNotCleanRelPath
	}
	return cleaned, nil
}
