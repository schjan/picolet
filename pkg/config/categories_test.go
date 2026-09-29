package config

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

// ApplyRank orders file writes: a pod is written before the containers that
// join it, so a partial apply never leaves a member without its pod file; a
// .image/.build likewise before the containers that name it (Image=), and
// after the networks and volumes a build may use.
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
		CategoryNetwork, CategoryVolume, CategoryImage, CategoryBuild, CategorySecret, CategorySystemd,
		CategoryManifest, CategoryFile, CategoryPod, CategoryContainer, CategoryKube,
	}, got)
}
