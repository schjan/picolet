package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/state"
	"github.com/schjan/picolet/pkg/status"
)

// pruneFailureCooldown bounds prune retries after a failure so a broken Podman
// socket cannot trigger a prune attempt on every tick.
const pruneFailureCooldown = time.Hour

// maybePruneImages removes unused container images when a prune is due. It is
// called from tick() and therefore runs in the agent's single-threaded loop —
// strictly serialized against ReconcileOnce/apply, so a prune can never race an
// image pull. On success it advances state.LastPrunedAt (persisted); on failure
// it leaves LastPrunedAt untouched and relies on the cooldown to bound retries.
func (a *Agent) maybePruneImages(ctx context.Context, st *state.State, store *state.Store) {
	// A non-positive interval means pruning is effectively off (production always
	// has a default via setDefaults; a zero interval only occurs for configs not
	// built through agentcfg.Parse).
	if a.dryRun || !a.cfg.PruneImagesEnabled() || a.cfg.PruneInterval <= 0 {
		slog.Debug("image prune skipped", "reason", "disabled_or_dry_run")
		return
	}
	if time.Since(st.LastPrunedAt) < a.cfg.PruneInterval {
		slog.Debug("image prune not due", "last_pruned_at", st.LastPrunedAt, "interval", a.cfg.PruneInterval)
		return
	}
	if !a.lastPruneAttemptAt.IsZero() && time.Since(a.lastPruneAttemptAt) < pruneFailureCooldown {
		slog.Debug("image prune backing off after recent failure", "last_attempt", a.lastPruneAttemptAt)
		return
	}
	a.lastPruneAttemptAt = time.Now()

	res, err := a.podman.ImagePrune(ctx, true)
	if err != nil {
		// Leave LastPrunedAt unadvanced so the prune is retried; the cooldown
		// above bounds the retry cadence.
		slog.Error("image prune failed", "error", err)
		metrics.ImagePruneTotal.WithLabelValues("error").Inc()
		a.statusStore.SetPrune(status.PruneStatus{LastRunAt: time.Now(), Error: err.Error()})
		return
	}

	st.LastPrunedAt = time.Now().Truncate(time.Second)
	if saveErr := store.Save(st); saveErr != nil {
		slog.Error("saving state after image prune", "error", saveErr)
	}
	slog.Info("image prune complete", "images_removed", res.ImagesRemoved, "reclaimed_bytes", res.ReclaimedBytes)
	metrics.ImagePruneTotal.WithLabelValues("success").Inc()
	metrics.ImagesPrunedTotal.Add(float64(res.ImagesRemoved))
	metrics.ImagePruneReclaimedBytesTotal.Add(float64(res.ReclaimedBytes))
	a.statusStore.SetPrune(status.PruneStatus{
		LastRunAt:      st.LastPrunedAt,
		ImagesRemoved:  res.ImagesRemoved,
		ReclaimedBytes: res.ReclaimedBytes,
	})
	// Deliberately no recordEvent: the 50-slot event ring (status.go) is reserved
	// for reconcile-lifecycle events; a periodic prune would evict them.
}
