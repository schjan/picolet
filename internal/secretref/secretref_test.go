package secretref_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/secretref"
	op "github.com/schjan/picolet/pkg/onepassword"
	pp "github.com/schjan/picolet/pkg/protonpass"
)

// provider is one secret-reference scheme, reached through its package's
// exported parser so each provider's wiring (prefix, scope label) is pinned
// by the same cases.
type provider struct {
	scheme string // "op" or "pass"
	scope  string // "vault" or "share"
	other  string // the other provider's scheme
	parse  func(string) (secretref.Ref, error)
	isRef  func(string) bool
}

// TestParse runs every case against both providers: $s stands for the
// provider's scheme, $o for the other provider's, $scope for its scope label
// and $ref for the quoted ref, so each error text is checked byte for byte
// as each provider printed it before the parsers were shared.
//
//nolint:funlen // table-driven test
func TestParse(t *testing.T) {
	t.Parallel()
	providers := []provider{
		{scheme: "op", scope: "vault", other: "pass", parse: op.ParseOpRef, isRef: op.IsRef},
		{scheme: "pass", scope: "share", other: "op", parse: pp.ParseRef, isRef: pp.IsRef},
	}
	tests := []struct {
		name     string
		ref      string
		want     secretref.Ref
		wantName string
		wantErr  string
	}{
		{
			name:     "simple reference",
			ref:      "$s://$scope/item/field",
			want:     secretref.Ref{Scope: "$scope", Item: "item", Field: "field"},
			wantName: "$scope_item_field",
		},
		{
			name:     "nested field with slashes",
			ref:      "$s://$scope/item/section/field",
			want:     secretref.Ref{Scope: "$scope", Item: "item", Field: "section/field"},
			wantName: "$scope_item_section_field",
		},
		{
			name:     "real-world reference",
			ref:      "$s://Infrastructure/db-password/password",
			want:     secretref.Ref{Scope: "Infrastructure", Item: "db-password", Field: "password"},
			wantName: "Infrastructure_db-password_password",
		},
		{
			name:     "real-world ID-style reference",
			ref:      "$s://abc123def456/item789/password",
			want:     secretref.Ref{Scope: "abc123def456", Item: "item789", Field: "password"},
			wantName: "abc123def456_item789_password",
		},
		{name: "missing prefix", ref: "secrets/foo.yml", wantErr: "invalid $s:// reference $ref: missing $s:// prefix"},
		{name: "wrong scheme", ref: "other://vault/item/field", wantErr: "invalid $s:// reference $ref: missing $s:// prefix"},
		{name: "other provider scheme", ref: "$o://a/b/c", wantErr: "invalid $s:// reference $ref: missing $s:// prefix"},
		{name: "only scope and item", ref: "$s://$scope/item", wantErr: "invalid $s:// reference $ref: expected $s://$scope/item/field"},
		{name: "empty scope", ref: "$s:///item/field", wantErr: "invalid $s:// reference $ref: expected $s://$scope/item/field"},
		{name: "empty item", ref: "$s://$scope//field", wantErr: "invalid $s:// reference $ref: expected $s://$scope/item/field"},
		{name: "empty field", ref: "$s://$scope/item/", wantErr: "invalid $s:// reference $ref: expected $s://$scope/item/field"},
		{name: "empty string", ref: "", wantErr: "invalid $s:// reference $ref: missing $s:// prefix"},
	}

	for _, p := range providers {
		for _, tt := range tests {
			t.Run(p.scheme+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				sub := strings.NewReplacer("$scope", p.scope, "$s", p.scheme, "$o", p.other).Replace
				ref := sub(tt.ref)
				got, err := p.parse(ref)
				if tt.wantErr != "" {
					require.EqualError(t, err, strings.ReplaceAll(sub(tt.wantErr), "$ref", strconv.Quote(ref)))
					assert.False(t, p.isRef(ref))
					return
				}
				require.NoError(t, err)
				want := secretref.Ref{Scope: sub(tt.want.Scope), Item: tt.want.Item, Field: tt.want.Field}
				assert.Equal(t, want, got)
				assert.Equal(t, sub(tt.wantName), got.PodmanSecretName())
				assert.True(t, p.isRef(ref))
			})
		}
	}
}
