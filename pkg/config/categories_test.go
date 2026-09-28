package config

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

// ApplyRank orders file writes; the deployable categories keep the order
// picolet has always applied them in.
func TestApplyOrderOfDeployableCategories(t *testing.T) {
	t.Parallel()
	deployable := Deployable()
	var got []Category
	for _, c := range ApplyOrder() {
		if slices.Contains(deployable, c) {
			got = append(got, c)
		}
	}
	assert.Equal(t, []Category{
		CategoryNetwork, CategoryVolume, CategorySecret, CategorySystemd,
		CategoryManifest, CategoryFile, CategoryContainer, CategoryKube,
	}, got)
}
