package resolver

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/sops"
)

// sopsReaderMode says which SOPS reader a test resolve passes.
type sopsReaderMode int

const (
	sopsNoReader   sopsReaderMode = iota // the Agent without a sops: block
	sopsKeyless                          // validate, resolve (NewKeyless)
	sopsDecrypting                       // the Agent with the TEST-ONLY identity srv-1
)

// String names the mode in subtest names.
func (m sopsReaderMode) String() string {
	switch m {
	case sopsNoReader:
		return "no-reader"
	case sopsKeyless:
		return "keyless"
	case sopsDecrypting:
		return "decrypting"
	}
	return fmt.Sprintf("mode(%d)", int(m))
}

// resolveDirectSops resolves test-host of a one-host Fleet whose base
// assignment lists secrets (direct sops:// refs, file secrets) and whose
// Host declares the corpus recipient srv-1; files adds Fleet-root files and
// may replace hosts/test-host/host.yml.
func resolveDirectSops(t *testing.T, secrets []string, files map[string]string, mode sopsReaderMode, strict bool) (*ResolvedHost, error) {
	t.Helper()
	base := "  secrets:\n"
	for _, s := range secrets {
		base += "    - \"" + s + "\"\n"
	}
	extra := map[string]string{"hosts/test-host/host.yml": sopsHostYAML(t, "srv-1")}
	maps.Copy(extra, files)
	fsys := pathsFleetFS(base, extra)
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	rc := Config{FS: fsys, Config: cfg, Strict: strict}
	var r *Resolver
	switch mode {
	case sopsKeyless:
		r, err = NewKeyless(rc)
	case sopsDecrypting:
		identity, readErr := os.ReadFile(filepath.Join("..", "..", "testdata", "sops", "identities", "srv-1.agekey"))
		require.NoError(t, readErr)
		ids, parseErr := sops.ParseIdentities(identity)
		require.NoError(t, parseErr)
		rc.Sops = sops.NewReader(fsys, ids)
		r, err = New(rc)
	case sopsNoReader:
		r, err = New(rc)
	}
	require.NoError(t, err)
	return r.ResolveHost(t.Context(), "test-host")
}

// directSopsFiles are the corpus copies the direct-ref tests read.
func directSopsFiles(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"secrets/db.sops.yaml": sopsCorpusFile(t, "values.sops.yaml"),
		"secrets/tls.sops.pem": sopsCorpusFile(t, "tls.sops.pem"),
	}
}

// secretsByDest maps each resolved secret's DestPath to its content.
func secretsByDest(resolved *ResolvedHost) map[string]string {
	got := map[string]string{}
	for _, f := range resolved.Files {
		if f.Category == config.CategorySecret {
			got[f.DestPath] = f.Content
		}
	}
	return got
}

// A direct sops:// secret is named after its pointer's segments joined by
// "_", or for a whole-file ref after the file's base name before `.sops.`.
// Keyless it renders the placeholder, decrypting the value.
func TestDirectSopsSecretNaming(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, ref, dest, value string
	}{
		{name: "nested pointer", ref: "sops://secrets/db.sops.yaml#/db/password", dest: "secret:db_password", value: "hunter2-test"},
		{name: "list index", ref: "sops://secrets/db.sops.yaml#/tokens/0", dest: "secret:tokens_0", value: "alpha-token"},
		{name: "whole file", ref: "sops://secrets/tls.sops.pem", dest: "secret:tls"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolved, err := resolveDirectSops(t, []string{tt.ref}, directSopsFiles(t), sopsKeyless, false)
			require.NoError(t, err)
			assert.Equal(t, map[string]string{tt.dest: sops.Placeholder}, secretsByDest(resolved))

			resolved, err = resolveDirectSops(t, []string{tt.ref}, directSopsFiles(t), sopsDecrypting, false)
			require.NoError(t, err)
			got := secretsByDest(resolved)
			require.Contains(t, got, tt.dest)
			if tt.value != "" {
				assert.Equal(t, tt.value, got[tt.dest])
			} else {
				assert.True(t, strings.HasPrefix(got[tt.dest], "-----BEGIN"), "whole file decrypted")
			}
		})
	}
}

// Direct sops:// refs fail the resolve, naming the ref, on a name collision
// with any other secret destination, a segment no Podman secret name can
// hold, a non-leaf, and an "" or null leaf, keyless as well as decrypting;
// no error quotes a value.
func TestDirectSopsSecretErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		secrets []string
		want    []string
	}{
		{
			name:    "collision with a file secret",
			secrets: []string{"secrets/db_password.tmpl", "sops://secrets/db.sops.yaml#/db/password"},
			want:    []string{"destination collision for secret:db_password: secrets/db_password.tmpl, sops://secrets/db.sops.yaml#/db/password"},
		},
		{
			name:    "invalid segment",
			secrets: []string{"sops://secrets/db.sops.yaml#/hosts/srv-1.example.net:443/password"},
			want: []string{
				"sops://secrets/db.sops.yaml#/hosts/srv-1.example.net:443/password: invalid Podman secret name",
				"deliver the value through a secrets/<name>.tmpl file containing {{ readSopsSecret",
			},
		},
		{
			name:    "escaped slash segment",
			secrets: []string{"sops://secrets/db.sops.yaml#/odd_keys/a~1b"},
			want:    []string{`segment "a/b" can't appear in a Podman secret name`},
		},
		{
			name:    "non-leaf",
			secrets: []string{"sops://secrets/db.sops.yaml#/db"},
			want:    []string{"sops://secrets/db.sops.yaml#/db", `"/db" is a map; a string ref must end at a leaf`},
		},
		{
			name:    "empty leaf",
			secrets: []string{"sops://secrets/db.sops.yaml#/db/empty"},
			want:    []string{"sops://secrets/db.sops.yaml#/db/empty: empty value: a direct secrets: entry needs a non-empty value"},
		},
		{
			name:    "null leaf",
			secrets: []string{"sops://secrets/db.sops.yaml#/db/nothing"},
			want:    []string{"sops://secrets/db.sops.yaml#/db/nothing: empty value: a direct secrets: entry needs a non-empty value"},
		},
	}
	files := directSopsFiles(t)
	files["secrets/db_password.tmpl"] = "static"
	for _, tt := range tests {
		for _, mode := range []sopsReaderMode{sopsKeyless, sopsDecrypting} {
			t.Run(tt.name+"/"+mode.String(), func(t *testing.T) {
				t.Parallel()
				_, err := resolveDirectSops(t, tt.secrets, files, mode, false)
				require.Error(t, err)
				for _, want := range tt.want {
					require.ErrorContains(t, err, want)
				}
				for _, plain := range []string{"hunter2-test", "visible", "5432", "alpha-token"} {
					assert.NotContains(t, err.Error(), plain)
				}
			})
		}
	}
}

// Keyless, a direct ref gets the template refs' recipient checks: the Host
// must declare age_recipients:, and be among the file's Recipients.
func TestDirectSopsSecretKeylessRecipientChecks(t *testing.T) {
	t.Parallel()
	ref := "sops://secrets/db.sops.yaml#/db/password"
	tests := []struct {
		name       string
		recipients []string
		want       string
	}{
		{name: "no age_recipients", want: "Host test-host references " + ref + " but declares no age_recipients"},
		{name: "drift", recipients: []string{"srv-3"}, want: "secrets/db.sops.yaml: not a recipient: the file is not encrypted to Host test-host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := directSopsFiles(t)
			files["hosts/test-host/host.yml"] = sopsHostYAML(t, tt.recipients...)
			_, err := resolveDirectSops(t, []string{ref}, files, sopsKeyless, false)
			require.ErrorContains(t, err, ref)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

// Without a SOPS reader a direct sops:// ref fails the resolve with the
// no-identity error; it is never skipped like an unconfigured op:// ref.
func TestDirectSopsSecretWithoutReaderFails(t *testing.T) {
	t.Parallel()
	ref := "sops://secrets/db.sops.yaml#/db/password"
	resolved, err := resolveDirectSops(t, []string{ref}, directSopsFiles(t), sopsNoReader, false)
	require.Error(t, err)
	assert.Nil(t, resolved)
	require.ErrorContains(t, err, ref)
	require.ErrorContains(t, err, "no age identity configured; set sops.age_identity_path in the Agent config")
}

// Strict mode (bootstrap, validate's Agent-bundle pass) fails a direct
// sops:// ref with the SOPS fix, even with the keyless reader passed.
// Service Bundles cannot carry direct secrets: entries, so this seam is
// where the Agent-bundle pass's direct-ref branch is covered.
func TestDirectSopsSecretStrictFails(t *testing.T) {
	t.Parallel()
	ref := "sops://secrets/db.sops.yaml#/db/password"
	for _, mode := range []sopsReaderMode{sopsKeyless, sopsNoReader} {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()
			_, err := resolveDirectSops(t, []string{ref}, directSopsFiles(t), mode, true)
			require.ErrorContains(t, err, ref)
			require.ErrorContains(t, err, "SOPS values can't be used in strict mode; "+
				"deliver the value as a --secrets-dir file and point the Agent config at its path; "+
				"credentials needed to fetch the Fleet can't be SOPS values")
		})
	}
}
