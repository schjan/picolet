package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/state"
)

//nolint:funlen // table-driven test with explicit cases for every-tick retry semantics
func TestMergePendingHooksKeepsUnattemptedAndAddsFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		old    map[string]int
		result *applier.ApplyResult
		want   map[string]int
	}{
		{
			name:   "attempted+succeeded removed",
			old:    map[string]int{"hook-a": 1},
			result: &applier.ApplyResult{AttemptedHookNames: []string{"hook-a"}},
			want:   nil,
		},
		{
			name:   "empty inputs return nil (not map{} — omitempty must omit)",
			old:    nil,
			result: &applier.ApplyResult{},
			want:   nil,
		},
		{
			name: "attempted+failed_keep_running increments count",
			old:  map[string]int{"hook-a": 2},
			result: &applier.ApplyResult{
				AttemptedHookNames: []string{"hook-a"},
				PendingHookNames:   []string{"hook-a"},
			},
			want: map[string]int{"hook-a": 3},
		},
		{
			name: "previously-pending hook is attempted each tick",
			old:  map[string]int{"hook-a": 1, "hook-b": 1},
			result: &applier.ApplyResult{
				AttemptedHookNames: []string{"hook-a", "hook-b"}, // both attempted via every-tick retry
				PendingHookNames:   nil,                          // both succeeded
			},
			want: nil,
		},
		{
			name: "previously-pending hook fails again, count increments",
			old:  map[string]int{"hook-a": 1},
			result: &applier.ApplyResult{
				AttemptedHookNames: []string{"hook-a"},
				PendingHookNames:   []string{"hook-a"},
			},
			want: map[string]int{"hook-a": 2},
		},
		{
			name: "new failure added even when not previously pending",
			old:  map[string]int{},
			result: &applier.ApplyResult{
				AttemptedHookNames: []string{"hook-c"},
				PendingHookNames:   []string{"hook-c"},
			},
			want: map[string]int{"hook-c": 1},
		},
		{
			name:   "attempted+fallback_restart removed (restart already scheduled)",
			old:    map[string]int{"hook-a": 1},
			result: &applier.ApplyResult{AttemptedHookNames: []string{"hook-a"}, FallbackRestartedUnits: []string{"app.service"}},
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mergePendingHooks(tt.old, tt.result)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMergePendingUnitsClearsConvergedAndAddsFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-time.Hour)

	tests := []struct {
		name   string
		old    map[string]state.PendingUnit
		result *applier.ApplyResult
		want   map[string]state.PendingUnit
	}{
		{
			name:   "empty inputs return nil",
			old:    nil,
			result: &applier.ApplyResult{},
			want:   nil,
		},
		{
			name:   "restarted unit is cleared",
			old:    map[string]state.PendingUnit{"foo.service": {SHA: "old", Attempts: 3, FirstFailedAt: earlier, LastAttemptAt: earlier}},
			result: &applier.ApplyResult{RestartedUnits: []string{"foo.service"}},
			want:   nil,
		},
		{
			name:   "new failure added with attempt count 1",
			old:    nil,
			result: &applier.ApplyResult{FailedRestartUnits: []string{"foo.service"}},
			want:   map[string]state.PendingUnit{"foo.service": {SHA: "head", Attempts: 1, FirstFailedAt: now, LastAttemptAt: now}},
		},
		{
			name:   "repeated failure increments and preserves FirstFailedAt",
			old:    map[string]state.PendingUnit{"foo.service": {SHA: "old", Attempts: 3, FirstFailedAt: earlier, LastAttemptAt: earlier}},
			result: &applier.ApplyResult{FailedRestartUnits: []string{"foo.service"}},
			want:   map[string]state.PendingUnit{"foo.service": {SHA: "head", Attempts: 4, FirstFailedAt: earlier, LastAttemptAt: now}},
		},
		{
			name:   "unrelated pending unit carried forward unchanged",
			old:    map[string]state.PendingUnit{"bar.service": {SHA: "old", Attempts: 1, FirstFailedAt: earlier, LastAttemptAt: earlier}},
			result: &applier.ApplyResult{FailedRestartUnits: []string{"foo.service"}},
			want: map[string]state.PendingUnit{
				"bar.service": {SHA: "old", Attempts: 1, FirstFailedAt: earlier, LastAttemptAt: earlier},
				"foo.service": {SHA: "head", Attempts: 1, FirstFailedAt: now, LastAttemptAt: now},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mergePendingUnits(tt.old, tt.result, "head", now)
			assert.Equal(t, tt.want, got)
		})
	}
}
