package sops_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/sops"
)

// Every recipient of the fixture corpus (whose identities are published in
// the repository) is a published test recipient; another key is not.
func TestIsPublishedTestRecipient(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(corpusDir, "recipients.txt"))
	require.NoError(t, err)
	n := 0
	for line := range strings.Lines(string(data)) {
		name, key, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || strings.HasPrefix(name, "#") {
			continue
		}
		r, err := sops.ParseRecipient(key)
		require.NoError(t, err)
		assert.True(t, sops.IsPublishedTestRecipient(r), "corpus recipient %s", name)
		n++
	}
	assert.Equal(t, 5, n, "the corpus recipients the test reads")

	other, err := sops.ParseRecipient("age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p")
	require.NoError(t, err)
	assert.False(t, sops.IsPublishedTestRecipient(other))
}
