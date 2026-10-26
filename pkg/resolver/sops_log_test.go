package resolver

import (
	"errors"
	"log/slog"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testlog"
	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/sops"
)

// dynamicRefTmpl builds a readSopsSecret ref from a value readSopsData
// decrypted. The ref is first built when the render pass sees the real
// db/user (the collect pass sees a pending placeholder), so it misses the
// collected cache and is resolved on its own.
const dynamicRefTmpl = `{{- $pw := readSopsData "sops://secrets/db.sops.yaml#/db/password" -}}
{{- if eq (readSopsSecret "sops://secrets/db.sops.yaml#/db/user") "app" -}}
{{ readSopsSecret (printf "sops://secrets/db.sops.yaml#/tokens/%s" $pw) }}
{{- end }}`

// A ref built from a decrypted value reaches no log record: the resolver
// logs a dynamic ref's cache miss, with debug logging on, before any
// scrubbing.
func TestDynamicSopsRefNotLogged(t *testing.T) {
	t.Parallel()
	extra := map[string]string{
		"secrets/db.sops.yaml":     testsops.CorpusText(t, "values.sops.yaml"),
		"hosts/test-host/host.yml": sopsHostYAML(t, "srv-1"),
	}
	maps.Copy(extra, map[string]string{appTmpl: dynamicRefTmpl})
	fsys := pathsFleetFS(appTmplBase, extra)
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	logger, rec := testlog.New()
	r, err := New(Config{
		FS:     fsys,
		Config: cfg,
		Sops:   sops.NewReader(fsys, testsops.Identities(t, "srv-1")),
		Logger: logger,
	})
	require.NoError(t, err)

	_, err = r.ResolveHost(t.Context(), "test-host")
	require.ErrorIs(t, err, sops.ErrBadPointer, "the resolve reaches the dynamic ref")
	require.True(t, rec.Has(slog.LevelDebug, "cache miss, resolving individually"), "the dynamic ref missed the cache:\n%s", rec.String())
	testsops.AssertNoPlaintext(t, errors.New("log records:\n"+rec.String()))
}
