package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
	"github.com/schjan/picolet/pkg/resolver"
	"github.com/schjan/picolet/pkg/state"
)

func TestDiffBootstrapScopePreservesNonPicoletState(t *testing.T) {
	t.Parallel()
	files := []resolver.ResolvedFile{
		{
			DestPath:    "/etc/containers/systemd/picolet/picolet.container",
			Content:     "[Container]\nImage=picolet:new\n",
			Category:    config.CategoryContainer,
			ServiceName: "picolet.service",
		},
		{
			DestPath: "secret:picolet_config",
			Content:  "hostname: node-1\n",
			Category: config.CategorySecret,
		},
	}
	st := &state.State{
		ManagedFiles: map[string]state.ManagedFile{
			"/etc/containers/systemd/picolet/picolet.container": {Hash: "sha256:old", Category: config.CategoryContainer},
			"/etc/containers/systemd/picolet/old.container":     {Hash: "sha256:old", Category: config.CategoryContainer},
			"/etc/containers/systemd/picolet/app.container":     {Hash: "sha256:app", Category: config.CategoryContainer},
			"secret:app_config": {Hash: "sha256:app", Category: config.CategorySecret},
		},
		ServiceNames: map[string]string{
			"/etc/containers/systemd/picolet/picolet.container": "picolet.service",
			"/etc/containers/systemd/picolet/old.container":     "picolet.service",
			"/etc/containers/systemd/picolet/app.container":     "app.service",
		},
	}

	changeset := diffBootstrapScope(files, st, "picolet.service")

	assert.Equal(t, reconciler.ActionUpdate, actionForPath(t, changeset, "/etc/containers/systemd/picolet/picolet.container"))
	assert.Equal(t, reconciler.ActionDelete, actionForPath(t, changeset, "/etc/containers/systemd/picolet/old.container"))
	assert.Equal(t, reconciler.ActionCreate, actionForPath(t, changeset, "secret:picolet_config"))
	assertNoChangeForPath(t, changeset, "/etc/containers/systemd/picolet/app.container")
	assertNoChangeForPath(t, changeset, "secret:app_config")
}

func TestWaitForHealthBoundsHungProbe(t *testing.T) {
	t.Parallel()
	oldClient := healthHTTPClient
	healthHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	t.Cleanup(func() { healthHTTPClient = oldClient })

	start := time.Now()
	err := WaitForHealth(context.Background(), "127.0.0.1:1", "/health", 50*time.Millisecond)

	require.Error(t, err)
	require.ErrorContains(t, err, "did not report healthy")
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}

func TestProbeOnceDialsGivenAddr(t *testing.T) {
	t.Parallel()
	paths := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	err := probeOnce(context.Background(), srv.Client(), strings.TrimPrefix(srv.URL, "http://"), "health")

	require.NoError(t, err)
	assert.Equal(t, "/health", <-paths)
}

func TestHealthAddrFromResolvedProbesLoopback(t *testing.T) {
	t.Parallel()
	const unitContent = "[Container]\nImage=picolet\nSecret=picolet_config,target=/etc/picolet/config.yml\n"
	unit := parser.NewUnitFile()
	unit.Filename = "picolet.container"
	require.NoError(t, unit.Parse(unitContent))

	tests := []struct {
		name       string
		listenYAML string
		want       string
	}{
		{name: "unset config binds loopback", listenYAML: "", want: "127.0.0.1:9417"},
		{name: "rendered listen_addr", listenYAML: "listen_addr: 127.0.0.1:9418\n", want: "127.0.0.1:9418"},
		{name: "wildcard listen_addr is probed on loopback", listenYAML: "listen_addr: 0.0.0.0:9500\n", want: "127.0.0.1:9500"},
		{name: "metrics_port short form", listenYAML: "metrics_port: 9418\n", want: "127.0.0.1:9418"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := []resolver.ResolvedFile{
				{
					DestPath:    "/etc/containers/systemd/picolet/picolet.container",
					Content:     unitContent,
					Category:    config.CategoryContainer,
					ParsedUnit:  unit,
					ServiceName: "picolet.service",
				},
				{
					DestPath: "secret:picolet_config",
					Content:  "hostname: node-1\n" + tt.listenYAML,
					Category: config.CategorySecret,
				},
			}

			addr, err := healthAddrFromResolved(files, "picolet.service")

			require.NoError(t, err)
			assert.Equal(t, tt.want, addr)
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func actionForPath(t *testing.T, cs *reconciler.Changeset, path string) reconciler.Action {
	t.Helper()
	for _, change := range cs.Changes {
		if change.DestPath == path {
			return change.Action
		}
	}
	require.FailNow(t, "change not found", path)
	return ""
}

func assertNoChangeForPath(t *testing.T, cs *reconciler.Changeset, path string) {
	t.Helper()
	for _, change := range cs.Changes {
		require.NotEqual(t, path, change.DestPath)
	}
}
