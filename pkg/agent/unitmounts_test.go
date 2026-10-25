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

// unitMounts reads each mount line as Quadlet and Podman do.
func TestUnitMounts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		line string
		want []mount
	}{
		{line: "Volume=/data", want: []mount{{target: "/data"}}},
		{line: "Volume=:/data", want: []mount{{target: "/data"}}},
		{line: "Volume=/srv/a/:/data/", want: []mount{{source: mountSource{kind: sourceVolume, name: "/srv/a"}, target: "/data"}}},
		{line: "Volume=creds:/data:ro,z", want: []mount{{source: mountSource{kind: sourceVolume, name: "creds"}, target: "/data"}}},
		{
			line: "Volume=creds:/data:subpath=/a/b/,U,ro",
			want: []mount{{source: mountSource{kind: sourceVolume, name: "creds", subpath: "a/b", options: "U"}, target: "/data"}},
		},
		{line: "Volume=creds.volume:/data", want: []mount{{source: mountSource{kind: sourceVolumeUnit, name: "creds.volume"}, target: "/data"}}},
		{
			line: "Mount=type=volume,source=creds,volume-subpath=a,readonly,destination=/data",
			want: []mount{{source: mountSource{kind: sourceVolume, name: "creds", subpath: "a"}, target: "/data"}},
		},
		{
			line: `Mount="type=bind,src=/srv/a,dst=/data,idmap"`,
			want: []mount{{source: mountSource{kind: sourceVolume, name: "/srv/a", options: "idmap"}, target: "/data"}},
		},
		{line: "Mount=type=tmpfs,destination=/data", want: []mount{{target: "/data"}}},
		{line: "Mount=type=glob,src=/srv/*,ro,dst=/data", want: []mount{{source: mountSource{kind: sourceSpec, name: "type=glob,src=/srv/*,ro"}, target: "/data"}}},
		{line: "Mount=type=bind,src=/srv/a"},
		{line: "Tmpfs=/data:size=1m", want: []mount{{target: "/data"}}},
		{line: "Secret=tok", want: []mount{{source: mountSource{kind: sourceSecret, name: "tok"}, target: "/run/secrets/tok"}}},
		{line: "Secret=target=/data/tok,source=tok", want: []mount{{source: mountSource{kind: sourceSecret, name: "tok"}, target: "/data/tok"}}},
		{line: "Secret=tok,type=env"},
		{line: "Secret=a b,target=x", want: []mount{
			{source: mountSource{kind: sourceSecret, name: "a"}, target: "/run/secrets/a"},
			{source: mountSource{kind: sourceSecret, name: "b"}, target: "/run/secrets/x"},
		}},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, unitMounts(parseContainerUnit(t, tt.line+"\n")), tt.line)
	}
}

// mountsHolding returns every mount at the deepest target holding a path.
func TestMountsHolding(t *testing.T) {
	t.Parallel()
	a := mount{source: mountSource{kind: sourceVolume, name: "/srv/a"}, target: "/data"}
	b := mount{source: mountSource{kind: sourceVolume, name: "/srv/b"}, target: "/data"}
	sub := mount{source: mountSource{kind: sourceVolume, name: "/srv/sub"}, target: "/data/sub"}
	mounts := []mount{a, sub, b}

	held, rel := mountsHolding(mounts, "/data/x")
	assert.Equal(t, []mount{a, b}, held)
	assert.Equal(t, "x", rel)
	held, rel = mountsHolding(mounts, "/data/sub/x/")
	assert.Equal(t, []mount{sub}, held)
	assert.Equal(t, "x", rel)
	held, rel = mountsHolding(mounts, "/data")
	assert.Equal(t, []mount{a, b}, held)
	assert.Equal(t, ".", rel)
	held, _ = mountsHolding(mounts, "/database")
	assert.Empty(t, held)
}

// Mounts passed through PodmanArgs= are mounts too, read as podman run reads
// its flags (a .volume source there is a volume name, not a unit); a flag
// that mounts what the guard cannot read (--volumes-from) is opaque.
func TestUnitMountsReadsPodmanArgs(t *testing.T) {
	t.Parallel()
	data := []mount{{source: mountSource{kind: sourceVolume, name: "/srv/s"}, target: "/data"}}
	tests := []struct {
		line   string
		want   []mount
		opaque []string
	}{
		{line: "PodmanArgs=--volume /srv/s:/data", want: data},
		{line: "PodmanArgs=--volume=/srv/s:/data:ro", want: data},
		{line: `PodmanArgs="--volume=/srv/s:/data"`, want: data},
		{line: "PodmanArgs=-v /srv/s:/data", want: data},
		{line: "PodmanArgs=-v=/srv/s:/data", want: data},
		{line: "PodmanArgs=-v/srv/s:/data", want: data},
		{line: "PodmanArgs=-dv /srv/s:/data", want: data},
		{line: "PodmanArgs=--security-opt apparmor=unconfined --volume /srv/s:/data", want: data},
		{line: "PodmanArgs=--mount type=bind,src=/srv/s,dst=/data", want: data},
		{line: "PodmanArgs=--mount=type=bind,src=/srv/s,dst=/data,ro", want: data},
		{line: "PodmanArgs=--volume creds.volume:/data", want: []mount{{source: mountSource{kind: sourceVolume, name: "creds.volume"}, target: "/data"}}},
		{line: "PodmanArgs=--tmpfs /data:size=1m", want: []mount{{target: "/data"}}},
		{line: "PodmanArgs=--secret tok,target=/data/tok", want: []mount{{source: mountSource{kind: sourceSecret, name: "tok"}, target: "/data/tok"}}},
		{line: "PodmanArgs=--secret=tok,type=env"},
		{line: "PodmanArgs=--security-opt apparmor=unconfined"},
		{line: "PodmanArgs=-e v=1 -w /data"},
		{line: "PodmanArgs=--volume"},
		{line: "PodmanArgs=--volumes-from other", opaque: []string{"--volumes-from other"}},
		{line: "PodmanArgs=--volumes-from=other:ro", opaque: []string{"--volumes-from other:ro"}},
	}
	for _, tt := range tests {
		unit := parseContainerUnit(t, tt.line+"\n")
		assert.Equal(t, tt.want, unitMounts(unit), tt.line)
		assert.Equal(t, tt.opaque, opaqueMounts(unit), tt.line)
	}
}
