package sops_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/sops"
)

func TestParsePointer(t *testing.T) {
	t.Parallel()

	cases := map[string][]string{
		"":                         nil,
		"/db/password":             {"db", "password"},
		"/tokens/0":                {"tokens", "0"},
		"/hosts/srv-1.example.net": {"hosts", "srv-1.example.net"},
		"/odd_keys/a~1b":           {"odd_keys", "a/b"},
		"/odd_keys/m~0n":           {"odd_keys", "m~n"},
		"/~01":                     {"~1"}, // ~0 first, so "~01" is "~1", not "/"
		"/":                        {""},
		"/a//b":                    {"a", "", "b"},
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			p, err := sops.ParsePointer(in)
			require.NoError(t, err)
			assert.Equal(t, want, sops.PointerSegments(p))
			assert.Equal(t, in, p.String(), "String round-trips")
		})
	}
}

func TestParsePointerRejects(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"db/password", "/db~2", "/db~", "#/db", "/~~01", "/a~"} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := sops.ParsePointer(in)
			require.ErrorIs(t, err, sops.ErrBadPointer)
		})
	}
}

func TestIsSOPSPath(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"secrets/db.sops.yaml":          true,
		"db.sops.yml":                   true,
		"hosts/srv-1/app.sops.json":     true,
		"tls.sops.pem":                  true,
		"app.sops.env":                  true,
		"services/web/secrets.sops.ini": true,
		"a.b.sops.yaml":                 true,
		".sops.json":                    true,  // only .sops.yaml/.sops.yml are config files
		"db.sops.prod.yaml":             false, // .sops. must sit directly before the final extension
		"db.sops.yaml.bak":              false,
		"x.sops.tar.gz":                 false,
		".sops.yaml":                    false, // SOPS's config file
		".sops.yml":                     false,
		"dir/.sops.yaml":                false,
		"db.yaml":                       false,
		"db.sops":                       false, // no extension after .sops.
		"db.sops.":                      false,
		"sops.yaml":                     false,
		"db-sops.yaml":                  false,
		"x.sops.yaml/db.yaml":           false, // the infix counts in the base name only
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, sops.IsSOPSPath(in))
		})
	}
}

func TestParseRef(t *testing.T) {
	t.Parallel()

	r, err := sops.ParseRef("sops://secrets/db.sops.yaml#/db/password")
	require.NoError(t, err)
	assert.Equal(t, "secrets/db.sops.yaml", r.Path)
	assert.Equal(t, []string{"db", "password"}, sops.PointerSegments(r.Pointer))
	assert.False(t, r.WholeFile())

	whole, err := sops.ParseRef("sops://certs/tls.sops.pem")
	require.NoError(t, err)
	assert.Equal(t, "certs/tls.sops.pem", whole.Path)
	assert.True(t, whole.WholeFile())
}

func TestParseRefRejects(t *testing.T) {
	t.Parallel()

	cases := map[string]error{
		"op://vault/item/field":                nil, // wrong scheme: any error
		"sops://":                              nil,
		"sops://#/db":                          nil,
		"sops://secrets/db.yaml#/db/password":  sops.ErrNotSOPSPath,
		"sops://.sops.yaml#/creation_rules":    sops.ErrNotSOPSPath,
		"sops://secrets/db.sops.yaml#db":       sops.ErrBadPointer,
		"sops://secrets/db.sops.yaml#":         sops.ErrBadPointer,
		"sops://secrets/db.sops.yaml#/db/a~2b": sops.ErrBadPointer,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := sops.ParseRef(in)
			require.Error(t, err)
			if want != nil {
				require.ErrorIs(t, err, want)
			}
			assert.Contains(t, err.Error(), in, "the error names the ref")
		})
	}
}

func TestRefSecretName(t *testing.T) {
	t.Parallel()

	cases := map[string]string{ //nolint:gosec // G101: Secret References, not credentials
		"sops://secrets/db.sops.yaml#/db/password":                "db_password",
		"sops://secrets/db.sops.yaml#/tokens/0":                   "tokens_0",
		"sops://secrets/db.sops.yaml#/hosts/srv-1.example.net/pw": "hosts_srv-1.example.net_pw",
		"sops://certs/tls.sops.pem":                               "tls",
		"sops://hosts/srv-1/app.sops.env":                         "app",
		"sops://a.sops.b.sops.yaml":                               "a.sops.b", // the part before the final .sops.
		"sops://secrets/db.sops.yaml#/DB_PASSWORD":                "DB_PASSWORD",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			r, err := sops.ParseRef(in)
			require.NoError(t, err)
			got, err := r.SecretName()
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestRefSecretNameRejectsInvalidSegments(t *testing.T) {
	t.Parallel()

	cases := map[string]string{ //nolint:gosec // G101: Secret References, not credentials
		"sops://secrets/db.sops.yaml#/odd_keys/a~1b":               `"a/b"`,
		"sops://secrets/db.sops.yaml#/odd_keys/m~0n":               `"m~n"`,
		"sops://secrets/db.sops.yaml#/db/pass word":                `"pass word"`,
		"sops://secrets/db.sops.yaml#/db/a,b":                      `"a,b"`,
		"sops://secrets/db.sops.yaml#/db/a=b":                      `"a=b"`,
		"sops://secrets/db.sops.yaml#/db/":                         `""`,
		"sops://secrets/db.sops.yaml#/":                            `""`,
		"sops://secrets/db.sops.yaml#/db/pässword":                 `"pässword"`,
		"sops://secrets/db.sops.yaml#/-flag":                       `"-flag"`,
		"sops://secrets/.sops.json":                                `""`,
		"sops://secrets/my%20cert.sops.pem":                        `"my%20cert"`,
		"sops://secrets/db.sops.yaml#/" + strings.Repeat("a", 254): strings.Repeat("a", 254),
	}
	for in, segment := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			r, err := sops.ParseRef(in)
			require.NoError(t, err)
			_, err = r.SecretName()
			require.ErrorIs(t, err, sops.ErrInvalidSecretName)
			msg := err.Error()
			assert.Contains(t, msg, in, "names the ref")
			assert.Contains(t, msg, segment, "names the segment")
			assert.Contains(t, msg, "secrets/<name>.tmpl")
			assert.Contains(t, msg, "readSopsSecret")
		})
	}
}
