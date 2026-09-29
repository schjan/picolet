package config

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

// ApplyRank orders file writes: a pod is written before the containers that
// join it, so a partial apply never leaves a member without its pod file.
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
		CategoryManifest, CategoryFile, CategoryPod, CategoryContainer, CategoryKube,
	}, got)
}
