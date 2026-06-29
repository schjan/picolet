package applier

import (
	"errors"
	"testing"

	"github.com/containers/podman/v5/pkg/domain/entities/reports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAggregatePrune(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		reps      []*reports.PruneReport
		want      PruneResult
		wantErr   bool
		errSubstr string
	}{
		{
			name: "no reports",
			reps: nil,
			want: PruneResult{},
		},
		{
			name: "all removed",
			reps: []*reports.PruneReport{
				{Id: "a", Size: 100},
				{Id: "b", Size: 250},
			},
			want: PruneResult{ImagesRemoved: 2, ReclaimedBytes: 350},
		},
		{
			name: "partial failure still aggregates successes",
			reps: []*reports.PruneReport{
				{Id: "a", Size: 100},
				{Id: "b", Err: errors.New("in use")},
				{Id: "c", Size: 50},
			},
			want:      PruneResult{ImagesRemoved: 2, ReclaimedBytes: 150},
			wantErr:   true,
			errSubstr: "image b",
		},
		{
			name: "nil entries skipped",
			reps: []*reports.PruneReport{nil, {Id: "a", Size: 10}},
			want: PruneResult{ImagesRemoved: 1, ReclaimedBytes: 10},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := aggregatePrune(tc.reps)
			assert.Equal(t, tc.want, got)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
				return
			}
			require.NoError(t, err)
		})
	}
}
