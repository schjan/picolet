package agent

import (
	"testing"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseContainerUnit(t *testing.T, lines string) *parser.UnitFile {
	t.Helper()
	unit := parser.NewUnitFile()
	require.NoError(t, unit.Parse("[Container]\nImage=x\n"+lines))
	return unit
}

// unitMounts reads each mount line's target as Quadlet and Podman do, and
// its source as written, without the target.
func TestUnitMounts(t *testing.T) {
	t.Parallel()
	data := []mount{{source: "/srv/s", target: "/data", hostPath: true}}
	tests := []struct {
		line   string
		want   []mount
		opaque []string
	}{
		{line: "Volume=/data", want: []mount{{target: "/data"}}},
		{line: "Volume=:/data", want: []mount{{target: "/data"}}},
		{line: "Volume=/srv/s:/data/", want: data},
		{line: "Volume=%h/s:/data:ro,z", want: []mount{{source: "%h/s:ro,z", target: "/data", hostPath: true}}},
		{line: "Volume=creds:/data", want: []mount{{source: "creds", target: "/data"}}},
		{line: "Volume=creds.volume:/data", want: []mount{{source: "creds.volume", target: "/data", volumeUnit: "creds.volume"}}},
		{
			line: "Mount=type=volume,source=creds.volume,destination=/data",
			want: []mount{{source: "type=volume,source=creds.volume", target: "/data", volumeUnit: "creds.volume"}},
		},
		{line: `Mount="type=bind,src=/srv/s,dst=/data,ro"`, want: []mount{{source: "type=bind,src=/srv/s,ro", target: "/data", hostPath: true}}},
		{line: "Mount=type=tmpfs,destination=/data", want: []mount{{target: "/data"}}},
		{line: "Mount=type=glob,src=/srv/*,dst=/data", want: []mount{{source: "type=glob,src=/srv/*", target: "/data"}}},
		{line: "Tmpfs=/data:size=1m", want: []mount{{target: "/data"}}},
		{line: "Secret=tok", want: []mount{{source: "secret tok", target: "/run/secrets/tok", secret: "tok"}}},
		{line: "Secret=target=/data/tok,source=tok,mode=0400", want: []mount{{source: "secret source=tok,mode=0400", target: "/data/tok", secret: "tok"}}},
		{line: "Secret=tok,type=env"},
		{line: "PodmanArgs=--volume /srv/s:/data", want: data},
		{line: "PodmanArgs=-dv=/srv/s:/data", want: data},
		{line: "PodmanArgs=--security-opt apparmor=unconfined -v/srv/s:/data", want: data},
		{line: "PodmanArgs=--mount=type=bind,src=/srv/s,dst=/data", want: []mount{{source: "type=bind,src=/srv/s", target: "/data", hostPath: true}}},
		{line: "PodmanArgs=--volume creds.volume:/data", want: []mount{{source: "creds.volume", target: "/data"}}},
		{line: "PodmanArgs=--tmpfs /data", want: []mount{{target: "/data"}}},
		{line: "PodmanArgs=--secret tok,target=/data/tok", want: []mount{{source: "secret tok", target: "/data/tok", secret: "tok"}}},
		{line: "PodmanArgs=-e v=1 -w /data"},
		{line: "PodmanArgs=--volume"},
		{line: "PodmanArgs=--volumes-from=other:ro", opaque: []string{"--volumes-from other:ro"}},
	}
	for _, tt := range tests {
		mounts, opaque := unitMounts(parseContainerUnit(t, tt.line+"\n"))
		assert.Equal(t, tt.want, mounts, tt.line)
		assert.Equal(t, tt.opaque, opaque, tt.line)
	}
}

// mountsHolding returns every mount at the deepest target holding a path.
func TestMountsHolding(t *testing.T) {
	t.Parallel()
	a := mount{source: "/srv/a", target: "/data"}
	b := mount{source: "/srv/b", target: "/data"}
	sub := mount{source: "/srv/sub", target: "/data/sub"}
	mounts := []mount{a, sub, b}

	assert.Equal(t, []mount{a, b}, mountsHolding(mounts, "/data/x"))
	assert.Equal(t, []mount{sub}, mountsHolding(mounts, "/data/sub/x/"))
	assert.Equal(t, []mount{a, b}, mountsHolding(mounts, "/data"))
	assert.Empty(t, mountsHolding(mounts, "/database"))
}

// sameMountLayout compares the mount keys' values as written.
func TestSameMountLayout(t *testing.T) {
	t.Parallel()
	base := "Volume=/srv/s:/data\n"
	assert.True(t, sameMountLayout(parseContainerUnit(t, base), parseContainerUnit(t, base+"Pull=newer\n")))
	for _, line := range []string{"Volume=/srv/s:/data:ro", "Mount=type=tmpfs,dst=/x", "Tmpfs=/x", "Secret=tok", "PodmanArgs=--init", "WorkingDir=/x"} {
		assert.False(t, sameMountLayout(parseContainerUnit(t, base), parseContainerUnit(t, base+line+"\n")), line)
	}
}
