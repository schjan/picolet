package metrics_test

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/status"
)

func newPruneRegistry(t *testing.T) (*status.Store, *prometheus.Registry) {
	t.Helper()
	store := status.NewStore()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(metrics.NewLastImagePruneCollector(store)))
	return store, reg
}

func TestLastImagePruneCollector_AbsentUntilFirstPrune(t *testing.T) {
	t.Parallel()
	_, reg := newPruneRegistry(t)

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no series should be emitted before the first successful prune")
}

func TestLastImagePruneCollector_EmitsAfterSuccess(t *testing.T) {
	t.Parallel()
	store, reg := newPruneRegistry(t)
	store.SetPrune(status.PruneStatus{LastRunAt: time.Unix(1700000000, 0), ImagesRemoved: 2})

	expected := `
# HELP picolet_last_image_prune_timestamp Unix timestamp of the last successful image prune. Absent until the first prune.
# TYPE picolet_last_image_prune_timestamp gauge
picolet_last_image_prune_timestamp 1.7e+09
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected)))
}

func TestLastImagePruneCollector_FailureDoesNotEmit(t *testing.T) {
	t.Parallel()
	store, reg := newPruneRegistry(t)
	// A failed attempt records an error (LastErrorAt/Error) but leaves LastRunAt
	// zero, so it must not create the "last successful prune" series — otherwise a
	// failing prune would look healthy.
	store.SetPrune(status.PruneStatus{LastErrorAt: time.Now(), Error: "podman socket unavailable"})

	count, err := testutil.GatherAndCount(reg)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "a failed prune must not emit the last-success series")
}
