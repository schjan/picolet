package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:funlen // multiple sub-tests with setup; splitting would obscure the test intent
func TestDeduplicateConfigFiles(t *testing.T) {
	t.Parallel()

	t.Run("same src+volume+path with different restart_service deduplicates", func(t *testing.T) {
		t.Parallel()
		assignments := &Assignments{
			Base: AssignmentGroup{
				ConfigFiles: []ConfigFileSpec{
					{Src: "configfiles/app.conf.tmpl", Volume: "data", Path: "app.conf", RestartService: "service-a"},
				},
			},
			PiTypes: map[string]AssignmentGroup{
				"controller": {
					ConfigFiles: []ConfigFileSpec{
						{Src: "configfiles/app.conf.tmpl", Volume: "data", Path: "app.conf", RestartService: "service-b"},
					},
				},
			},
			Features: map[string]AssignmentGroup{},
		}

		result := assignments.Resolve(&HostConfig{PiType: "controller"})
		require.Len(t, result.ConfigFiles, 1, "duplicate src+volume+path should be deduplicated to one entry")
		// SortStableFunc preserves insertion order (base before pi_type); CompactFunc keeps first.
		assert.Equal(t, "service-a", result.ConfigFiles[0].RestartService, "base entry should be retained (stable sort preserves insertion order)")
	})

	t.Run("identical specs across base and feature deduplicate", func(t *testing.T) {
		t.Parallel()
		spec := ConfigFileSpec{Src: "configfiles/cfg.tmpl", Volume: "vol", Path: "cfg.conf", RestartService: "svc"}
		assignments := &Assignments{
			Base: AssignmentGroup{
				ConfigFiles: []ConfigFileSpec{spec},
			},
			PiTypes: map[string]AssignmentGroup{},
			Features: map[string]AssignmentGroup{
				"feat": {
					ConfigFiles: []ConfigFileSpec{spec},
				},
			},
		}

		result := assignments.Resolve(&HostConfig{Features: []string{"feat"}})
		require.Len(t, result.ConfigFiles, 1, "identical specs should be deduplicated to single entry")
		assert.Equal(t, spec, result.ConfigFiles[0])
	})

	t.Run("different specs are preserved", func(t *testing.T) {
		t.Parallel()
		assignments := &Assignments{
			Base: AssignmentGroup{
				ConfigFiles: []ConfigFileSpec{
					{Src: "configfiles/a.conf.tmpl", Volume: "vol", Path: "a.conf"},
					{Src: "configfiles/b.conf.tmpl", Volume: "vol", Path: "b.conf"},
				},
			},
			PiTypes:  map[string]AssignmentGroup{},
			Features: map[string]AssignmentGroup{},
		}

		result := assignments.Resolve(&HostConfig{})
		assert.Len(t, result.ConfigFiles, 2)
	})
}
