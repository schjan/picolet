package sops_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// corpusDir is the shared SOPS fixture corpus (regenerate with
// `task sops:corpus`).
var corpusDir = filepath.Join("..", "..", "testdata", "sops")

// corpusRecipient returns the TEST-ONLY public recipient called name from
// testdata/sops/recipients.txt.
func corpusRecipient(t *testing.T, name string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(corpusDir, "recipients.txt"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
	for sc.Scan() {
		n, key, ok := strings.Cut(sc.Text(), " ")
		if ok && n == name {
			return key
		}
	}
	require.NoError(t, sc.Err())
	t.Fatalf("recipient %q not in corpus", name)
	return ""
}

// corpusFile reads a corpus file by name.
func corpusFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(corpusDir, "corpus", name))
	require.NoError(t, err)
	return data
}
