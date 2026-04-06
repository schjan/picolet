package onepassword

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:funlen // table-driven test
func TestParseOpRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		ref        string
		want       OpRef
		wantName   string
		wantErrMsg string
	}{
		{
			name:     "simple reference",
			ref:      "op://vault/item/field",
			want:     OpRef{Vault: "vault", Item: "item", Field: "field"},
			wantName: "vault_item_field",
		},
		{
			name:     "nested field with slashes",
			ref:      "op://vault/item/section/field",
			want:     OpRef{Vault: "vault", Item: "item", Field: "section/field"},
			wantName: "vault_item_section_field",
		},
		{
			name:     "real-world reference",
			ref:      "op://Infrastructure/db-password/password",
			want:     OpRef{Vault: "Infrastructure", Item: "db-password", Field: "password"},
			wantName: "Infrastructure_db-password_password",
		},
		{
			name:       "missing prefix",
			ref:        "secrets/foo.yml",
			wantErrMsg: "missing op:// prefix",
		},
		{
			name:       "only vault and item",
			ref:        "op://vault/item",
			wantErrMsg: "expected op://vault/item/field",
		},
		{
			name:       "empty vault",
			ref:        "op:///item/field",
			wantErrMsg: "expected op://vault/item/field",
		},
		{
			name:       "empty item",
			ref:        "op://vault//field",
			wantErrMsg: "expected op://vault/item/field",
		},
		{
			name:       "empty field",
			ref:        "op://vault/item/",
			wantErrMsg: "expected op://vault/item/field",
		},
		{
			name:       "empty string",
			ref:        "",
			wantErrMsg: "missing op:// prefix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseOpRef(tt.ref)
			if tt.wantErrMsg != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantErrMsg)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantName, got.PodmanSecretName())
		})
	}
}
