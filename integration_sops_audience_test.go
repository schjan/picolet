package picolet_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/validator"
)

// sopsOperatorsYML is an operators.yml with one Operator per corpus
// recipient called names (Operator name = recipient name).
func sopsOperatorsYML(t *testing.T, names ...string) *fstest.MapFile {
	t.Helper()
	y := ""
	for _, n := range names {
		y += n + ":\n  age_recipients:\n    - " + testsops.Recipient(t, n) + "\n"
	}
	return data([]byte(y))
}

// corpusRecipientNames are the names of every corpus recipient.
var corpusRecipientNames = []string{"srv-1", "srv-2", "srv-3", "operator-1", "srv-pq"}

// corpusOperatorsExcept is sopsOperatorsYML of every corpus recipient but
// hostRecipients (which a Host declares, so no Operator may).
func corpusOperatorsExcept(t *testing.T, hostRecipients []string) *fstest.MapFile {
	t.Helper()
	var names []string
	for _, n := range corpusRecipientNames {
		if !strings.Contains(" "+strings.Join(hostRecipients, " ")+" ", " "+n+" ") {
			names = append(names, n)
		}
	}
	return sopsOperatorsYML(t, names...)
}

// audienceFleet is a two-Host Fleet: web-a (recipient srv-1) reads
// secrets/shared.sops.yaml (srv-1, srv-2, operator-1) through a templated
// readSopsSecret ref, db-b (srv-2) through a direct secrets: entry; the
// Operator operator-1 holds operator-1. It passes validate.
func audienceFleet(t *testing.T) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{
		"fleet.yml": data([]byte("images: {}\nports: {picolet_system_metrics: 9418}\n")),
		"assignments.yml": data([]byte("base: {}\nroles:\n" +
			"  web:\n    secrets: [secrets/web.yml.tmpl]\n" +
			"  db:\n    secrets: [\"sops://secrets/shared.sops.yaml#/tokens/0\"]\n" +
			"features: {}\n")),
		"hosts/web-a/host.yml":     data([]byte("hostname: web-a\nrole: web\nage_recipients:\n  - " + testsops.Recipient(t, "srv-1") + "\n")),
		"hosts/db-b/host.yml":      data([]byte("hostname: db-b\nrole: db\nage_recipients:\n  - " + testsops.Recipient(t, "srv-2") + "\n")),
		"secrets/web.yml.tmpl":     data([]byte(`password: {{ readSopsSecret (printf "sops://secrets/%s.sops.yaml#/db/password" "shared") }}` + "\n")),
		"secrets/shared.sops.yaml": data(testsops.CorpusFile(t, "values.sops.yaml")),
		config.OperatorsFile:       sopsOperatorsYML(t, "operator-1"),
	}
}

// audienceFixes is the remediation every audience error gives for path.
func audienceFixes(path string) string {
	return "planned retirement: drop it from the `.sops.yaml` rule and run `sops updatekeys " + path + "`; " +
		"trust lost: drop it, run `sops rotate -i " + path + "` (new data key) and change every value. " +
		"`updatekeys` revokes nothing"
}

func TestIntegrationValidateSOPSAudiencePasses(t *testing.T) {
	t.Parallel()
	require.NoError(t, validateFleet(t, audienceFleet(t)),
		"every Recipient is an Operator or a Host that reaches the file (templated or direct ref)")
}

func TestIntegrationValidateSOPSAudienceFails(t *testing.T) {
	t.Parallel()
	const shared = "secrets/shared.sops.yaml"
	srv1, srv2, op := testsops.Recipient(t, "srv-1"), testsops.Recipient(t, "srv-2"), testsops.Recipient(t, "operator-1")
	tests := []struct {
		name   string
		change func(fstest.MapFS)
		want   string
	}{
		{
			name: "cross-Host leak",
			change: func(fsys fstest.MapFS) {
				fsys["assignments.yml"] = data([]byte("base: {}\nroles:\n  web:\n    secrets: [secrets/web.yml.tmpl]\nfeatures: {}\n"))
			},
			want: "`" + shared + "`: age recipient " + srv2 + " (owner: Host db-b) may not read it: " +
				"it is not a Recipient of a Host that references the file or of an Operator; " + audienceFixes(shared),
		},
		{
			name: "unknown Recipient",
			change: func(fsys fstest.MapFS) {
				delete(fsys, "hosts/db-b/host.yml")
			},
			want: "`" + shared + "`: age recipient " + srv2 + " (owner: unknown) may not read it",
		},
		{
			name: "Operator not declared",
			change: func(fsys fstest.MapFS) {
				delete(fsys, config.OperatorsFile)
			},
			want: "`" + shared + "`: age recipient " + op + " (owner: unknown) may not read it",
		},
		{
			name: "unreferenced file",
			change: func(fsys fstest.MapFS) {
				fsys["secrets/old.sops.yaml"] = data(testsops.CorpusFile(t, "encrypted-regex.sops.yaml")) // srv-1
			},
			want: "`secrets/old.sops.yaml`: age recipient " + srv1 + " (owner: Host web-a) may not read it: " +
				"no Host references the file, so only Operators may read it; " + audienceFixes("secrets/old.sops.yaml"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := audienceFleet(t)
			tt.change(fsys)
			err := validateFleet(t, fsys)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			testsops.AssertNoPlaintext(t, err)
		})
	}
}

// With a Host failing to resolve, the audience would be incomplete: the
// pass is skipped, and the note says why.
func TestIntegrationValidateSOPSAudienceSkippedOnResolveFailure(t *testing.T) {
	t.Parallel()
	fsys := audienceFleet(t)
	// The leak of the "cross-Host leak" case, plus a Host whose ref fails.
	fsys["assignments.yml"] = data([]byte("base: {}\nroles:\n  web:\n    secrets: [secrets/web.yml.tmpl]\n" +
		"  broken:\n    secrets: [secrets/broken.yml.tmpl]\nfeatures: {}\n"))
	fsys["hosts/broken-c/host.yml"] = data([]byte("hostname: broken-c\nrole: broken\nage_recipients:\n  - " + testsops.Recipient(t, "srv-3") + "\n"))
	fsys["secrets/broken.yml.tmpl"] = data([]byte(`x: {{ readSopsSecret "sops://secrets/gone.sops.yaml#/x" }}` + "\n"))

	var logs bytes.Buffer
	err := validateFleet(t, fsys, validator.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	require.ErrorContains(t, err, "host broken-c: resolve")
	assert.NotContains(t, err.Error(), "may not read it")
	testsops.AssertNoPlaintext(t, err)
	assert.Contains(t, logs.String(), "skipping the SOPS audience pass")
	assert.Contains(t, logs.String(), "broken-c")
}

// A published TEST-ONLY recipient declared by a Host or an Operator is
// warned about, unless the validate options exempt it (as test Fleets do).
func TestIntegrationValidateWarnsOnPublishedTestRecipients(t *testing.T) {
	t.Parallel()
	fsys := audienceFleet(t)
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := resolver.NewKeyless(resolver.Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	require.NoError(t, validator.ValidateAll(t.Context(), fsys, r, cfg, validator.WithLogger(log)))
	for _, want := range []string{"host=web-a", "host=db-b", "operator=operator-1", testsops.Recipient(t, "srv-1")} {
		assert.Contains(t, logs.String(), want)
	}
	assert.Contains(t, logs.String(), "published test recipient")

	logs.Reset()
	require.NoError(t, validator.ValidateAll(t.Context(), fsys, r, cfg, validator.WithLogger(log), validator.AllowTestRecipients()))
	assert.NotContains(t, logs.String(), "published test recipient")
}

// A Host's published TEST-ONLY recipient is warned about even when
// operators.yml fails to load.
func TestIntegrationValidateWarnsOnPublishedTestRecipientsWhenOperatorsFail(t *testing.T) {
	t.Parallel()
	fsys := audienceFleet(t)
	fsys["operators.yml"] = data([]byte("operator-1:\n  age_recipients: [not-a-recipient]\n"))
	cfg, err := config.LoadAll(fsys)
	require.NoError(t, err)
	r, err := resolver.NewKeyless(resolver.Config{FS: fsys, Config: cfg})
	require.NoError(t, err)

	var logs bytes.Buffer
	err = validator.ValidateAll(t.Context(), fsys, r, cfg, validator.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	require.ErrorContains(t, err, "operators.yml")
	assert.Contains(t, logs.String(), "published test recipient")
	assert.Contains(t, logs.String(), "host=web-a")
}
