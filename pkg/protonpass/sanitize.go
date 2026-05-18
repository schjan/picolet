package protonpass

import (
	"strings"
	"unicode"
)

const maxSanitizedStderrBytes = 200

// sanitizeStderr returns a length-limited, secret-safe representation of
// pass-cli's stderr output suitable for inclusion in error messages or logs.
//
// pass-cli stderr may contain partial secret material on parser failures or
// crash dumps. We replace any run of characters that looks like a base64- or
// hex-encoded token (length >= 24, alphanumeric/+/=/-/_) with "<redacted>",
// and truncate the result to maxSanitizedStderrBytes.
func sanitizeStderr(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(b))
	s = redactTokenLikeRuns(s)
	if len(s) > maxSanitizedStderrBytes {
		s = s[:maxSanitizedStderrBytes] + "…"
	}
	return s
}

// redactTokenLikeRuns scans s for contiguous runs of letters and digits
// (the body of a base64/hex token) and replaces any run of length >= 24 with
// "<redacted>". Punctuation and separators (=, _, -, +, /) are deliberately
// excluded so that constructs like "key=value" stay readable; only the
// large opaque blob inside is redacted.
func redactTokenLikeRuns(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	runStart := -1
	flush := func(end int) {
		if runStart < 0 {
			return
		}
		runLen := end - runStart
		if runLen >= 24 {
			b.WriteString("<redacted>")
		} else {
			b.WriteString(s[runStart:end])
		}
		runStart = -1
	}

	for i, r := range s {
		if isTokenRune(r) {
			if runStart < 0 {
				runStart = i
			}
			continue
		}
		flush(i)
		b.WriteRune(r)
	}
	flush(len(s))
	return b.String()
}

func isTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
