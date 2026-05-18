package protonpass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:funlen // table-driven test
func TestParseRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		ref        string
		want       PassRef
		wantName   string
		wantErrMsg string
	}{
		{
			name:     "simple reference",
			ref:      "pass://share/item/field",
			want:     PassRef{Share: "share", Item: "item", Field: "field"},
			wantName: "share_item_field",
		},
		{
			name:     "nested field with slashes",
			ref:      "pass://share/item/section/field",
			want:     PassRef{Share: "share", Item: "item", Field: "section/field"},
			wantName: "share_item_section_field",
		},
		{
			name:     "real-world ID-style reference",
			ref:      "pass://abc123def456/item789/password",
			want:     PassRef{Share: "abc123def456", Item: "item789", Field: "password"},
			wantName: "abc123def456_item789_password",
		},
		{
			name:       "missing prefix",
			ref:        "secrets/foo.yml",
			wantErrMsg: "missing pass:// prefix",
		},
		{
			name:       "wrong scheme (op://)",
			ref:        "op://vault/item/field",
			wantErrMsg: "missing pass:// prefix",
		},
		{
			name:       "only share and item",
			ref:        "pass://share/item",
			wantErrMsg: "expected pass://share/item/field",
		},
		{
			name:       "empty share",
			ref:        "pass:///item/field",
			wantErrMsg: "expected pass://share/item/field",
		},
		{
			name:       "empty item",
			ref:        "pass://share//field",
			wantErrMsg: "expected pass://share/item/field",
		},
		{
			name:       "empty field",
			ref:        "pass://share/item/",
			wantErrMsg: "expected pass://share/item/field",
		},
		{
			name:       "empty string",
			ref:        "",
			wantErrMsg: "missing pass:// prefix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRef(tt.ref)
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

func TestIsRef(t *testing.T) {
	t.Parallel()
	assert.True(t, IsRef("pass://a/b/c"))
	assert.False(t, IsRef("op://a/b/c"))
	assert.False(t, IsRef("not-a-ref"))
	assert.False(t, IsRef(""))
}
