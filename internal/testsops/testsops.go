// Package testsops holds what tests share about picolet's TEST-ONLY SOPS
// fixture corpus (testdata/sops, regenerated with `task sops:corpus`): its
// files, recipients and identities, and the leak assertion over its
// plaintext values. It imports pkg/sops, so only pkg/sops's external tests
// (package sops_test) may use it there.
package testsops

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/sops"
)

// Dir is testdata/sops, as an absolute path, so tests in any package find it.
var Dir = func() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("testsops: cannot locate testdata/sops")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "sops")
}()

// Read reads the file at rel below testdata/sops (corpus/<name>,
// recipients.txt, plain/<name>, …).
func Read(tb testing.TB, rel string) []byte {
	tb.Helper()
	data, err := os.ReadFile(filepath.Join(Dir, rel))
	require.NoError(tb, err)
	return data
}

// CorpusFile reads the encrypted corpus file called name
// (testdata/sops/corpus/<name>).
func CorpusFile(tb testing.TB, name string) []byte {
	tb.Helper()
	return Read(tb, filepath.Join("corpus", name))
}

// CorpusText is CorpusFile as a string.
func CorpusText(tb testing.TB, name string) string {
	tb.Helper()
	return string(CorpusFile(tb, name))
}

// Recipient returns the TEST-ONLY public recipient called name from
// testdata/sops/recipients.txt.
func Recipient(tb testing.TB, name string) string {
	tb.Helper()
	key, ok := lookupRecipient(string(Read(tb, "recipients.txt")), name)
	if !ok {
		tb.Fatalf("recipient %q not in corpus", name)
	}
	return key
}

// RecipientOf is Recipient outside a test (for tables); it panics when the
// recipient is missing.
func RecipientOf(name string) string {
	data, err := os.ReadFile(filepath.Join(Dir, "recipients.txt"))
	if err != nil {
		panic(err)
	}
	key, ok := lookupRecipient(string(data), name)
	if !ok {
		panic("recipient " + name + " not in corpus")
	}
	return key
}

func lookupRecipient(recipients, name string) (string, bool) {
	for line := range strings.Lines(recipients) {
		if n, key, ok := strings.Cut(strings.TrimSpace(line), " "); ok && n == name {
			return key, true
		}
	}
	return "", false
}

// IdentityFile returns the TEST-ONLY identity files called names
// (testdata/sops/identities/<name>.agekey) as one identity file holding
// them all.
func IdentityFile(tb testing.TB, names ...string) []byte {
	tb.Helper()
	var data []byte
	for _, name := range names {
		data = append(data, Read(tb, filepath.Join("identities", name+".agekey"))...)
	}
	return data
}

// Identities parses the TEST-ONLY identities called names into one set, as
// an Agent's identity file holding several keys would be.
func Identities(tb testing.TB, names ...string) *sops.Identities {
	tb.Helper()
	ids, err := sops.ParseIdentities(IdentityFile(tb, names...))
	require.NoError(tb, err)
	return ids
}

// Plaintexts are the values of testdata/sops/plain/values.yaml long enough
// to be recognisable in an error.
var Plaintexts = []string{
	"hunter2-test", "alpha-token", "beta-token", "gamma-secret", "srv1-pass",
	"slash-key", "tilde-key", "5432", "0.75", "visible",
}

// HexRun matches a run of hex digits as long as a truncated hash; SOPS's MAC
// is a 128-digit SHA-512.
var HexRun = regexp.MustCompile(`[0-9A-Fa-f]{32,}`)

// AssertNoPlaintext fails unless err is an error that carries no fixture
// plaintext (Plaintexts plus extra) and nothing that looks like a hash of
// one.
func AssertNoPlaintext(tb testing.TB, err error, extra ...string) {
	tb.Helper()
	require.Error(tb, err)
	msg := err.Error()
	for _, p := range append(append([]string(nil), Plaintexts...), extra...) {
		require.NotContains(tb, msg, p, "error leaks a fixture plaintext")
	}
	require.Empty(tb, HexRun.FindAllString(msg, -1), "error carries a hash-like hex run")
}
