package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/schjan/picolet/pkg/agentcfg"
	"github.com/schjan/picolet/pkg/resolver"
)

// Intentional structural twin of TestPpRefreshDue below; collapsing into a
// parametrized helper would have to thread the method (opRefreshDue vs
// ppRefreshDue) and field-set callbacks through a closure, which obscures the
// per-provider symmetry the tests are documenting.
//
//nolint:dupl // see comment above
func TestOpRefreshDue(t *testing.T) {
	t.Parallel()

	dummyReader := resolver.SecretRefReader(func(_ context.Context, _ []string) (map[string]string, error) {
		return nil, nil //nolint:nilnil // test stub
	})
	opCfg := &agentcfg.OnePasswordConfig{RefreshInterval: 10 * time.Minute}

	tests := []struct {
		name          string
		opReader      resolver.SecretRefReader
		onePassword   *agentcfg.OnePasswordConfig
		lastOPRefresh time.Time
		want          bool
	}{
		{
			name:     "nil opReader always returns false",
			opReader: nil,
			// cfg.OnePassword populated to prove the nil-reader guard triggers first.
			onePassword: opCfg,
			want:        false,
		},
		{
			name:          "zero lastOPRefresh returns true (first run)",
			opReader:      dummyReader,
			onePassword:   opCfg,
			lastOPRefresh: time.Time{},
			want:          true,
		},
		{
			name:          "interval not yet elapsed returns false",
			opReader:      dummyReader,
			onePassword:   opCfg,
			lastOPRefresh: time.Now().Add(-5 * time.Minute), // < 10m interval
			want:          false,
		},
		{
			name:          "interval elapsed returns true",
			opReader:      dummyReader,
			onePassword:   opCfg,
			lastOPRefresh: time.Now().Add(-11 * time.Minute), // > 10m interval
			want:          true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &Agent{
				opReader: tc.opReader,
				cfg: &agentcfg.Config{
					Hostname:    "test-host",
					OnePassword: tc.onePassword,
				},
				lastOPRefresh: tc.lastOPRefresh,
			}
			assert.Equal(t, tc.want, a.opRefreshDue())
		})
	}
}

//nolint:dupl // structural twin of TestOpRefreshDue; see comment there
func TestPpRefreshDue(t *testing.T) {
	t.Parallel()

	dummyReader := resolver.SecretRefReader(func(_ context.Context, _ []string) (map[string]string, error) {
		return nil, nil //nolint:nilnil // test stub
	})
	ppCfg := &agentcfg.ProtonPassConfig{RefreshInterval: 10 * time.Minute}

	tests := []struct {
		name          string
		ppReader      resolver.SecretRefReader
		protonPass    *agentcfg.ProtonPassConfig
		lastPPRefresh time.Time
		want          bool
	}{
		{
			name:       "nil ppReader always returns false",
			ppReader:   nil,
			protonPass: ppCfg, // populated to prove the nil-reader guard triggers first
			want:       false,
		},
		{
			name:          "zero lastPPRefresh returns true (first run)",
			ppReader:      dummyReader,
			protonPass:    ppCfg,
			lastPPRefresh: time.Time{},
			want:          true,
		},
		{
			name:          "interval not yet elapsed returns false",
			ppReader:      dummyReader,
			protonPass:    ppCfg,
			lastPPRefresh: time.Now().Add(-5 * time.Minute),
			want:          false,
		},
		{
			name:          "interval elapsed returns true",
			ppReader:      dummyReader,
			protonPass:    ppCfg,
			lastPPRefresh: time.Now().Add(-11 * time.Minute),
			want:          true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &Agent{
				ppReader: tc.ppReader,
				cfg: &agentcfg.Config{
					Hostname:   "test-host",
					ProtonPass: tc.protonPass,
				},
				lastPPRefresh: tc.lastPPRefresh,
			}
			assert.Equal(t, tc.want, a.ppRefreshDue())
		})
	}
}
