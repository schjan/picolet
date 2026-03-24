package onepassword

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateReference(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ref  string
		want bool
	}{
		{"valid ref", "op://vault/item/field", true},
		{"valid with section", "op://vault/item/section/field", true},
		{"missing prefix", "vault/item/field", false},
		{"empty", "", false},
		{"wrong prefix", "secret://vault/item", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ValidateReference(tt.ref))
		})
	}
}
