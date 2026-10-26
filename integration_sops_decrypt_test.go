package picolet_test

import (
	"os"
	"testing"

	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/sops"
)

// TestIntegrationResolveGoldenDecrypting is TestIntegrationResolveGolden the
// Agent's way: each Host resolves with the decrypting reader and the
// TEST-ONLY identity srv-1. It snapshots every file whose keyless render
// used a SOPS value (ResolvedFile.SopsValues: string placeholders and typed
// ones alike), so the goldens show the real (obviously fake, TEST-ONLY)
// values and Podman secrets.
func TestIntegrationResolveGoldenDecrypting(t *testing.T) {
	t.Parallel()
	repoFS := os.DirFS(testdataDir)
	cfg, err := config.LoadAll(repoFS)
	require.NoError(t, err)
	ids := testsops.Identities(t, "srv-1")

	keyless, err := resolver.NewKeyless(resolver.Config{FS: repoFS, Config: cfg})
	require.NoError(t, err)
	g := goldie.New(t, goldie.WithFixtureDir("testdata/fixtures/decrypt"))

	snapshots := 0
	for _, hostname := range cfg.SortedHostnames() {
		placeholders, err := keyless.ResolveHost(t.Context(), hostname)
		require.NoError(t, err)
		withSops := map[string]bool{}
		for _, f := range placeholders.Files {
			if f.SopsValues {
				withSops[f.DestPath] = true
			}
		}
		if len(withSops) == 0 {
			continue
		}

		// A fresh decrypting reader per host resolve, as on the Agent.
		r, err := resolver.New(resolver.Config{FS: repoFS, Config: cfg, Sops: sops.NewReader(repoFS, ids)})
		require.NoError(t, err)
		resolved, err := r.ResolveHost(t.Context(), hostname)
		require.NoError(t, err)
		for _, f := range resolved.Files {
			if !withSops[f.DestPath] {
				continue
			}
			assert.Contains(t, f.Content, "TEST-ONLY", "%s: example Fleet values are obviously fake", f.DestPath)
			g.Assert(t, hostname+"/"+sanitizePath(f.DestPath), []byte(f.Content))
			snapshots++
		}
	}
	require.NotZero(t, snapshots, "the example Fleet renders at least one SOPS value")
}
