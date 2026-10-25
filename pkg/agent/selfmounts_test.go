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

// translate finds where the running container shows a path of the next
// start's mount: under a mount with the same source, unless shadowed.
func TestTranslate(t *testing.T) {
	t.Parallel()
	creds := mountSource{kind: sourceVolume, name: "/srv/creds"}
	next := mount{source: creds, target: "/new"}
	at, shadow := translate(next, "hook", []mount{{source: creds, target: "/old"}})
	assert.Equal(t, "/old/hook", at)
	assert.Nil(t, shadow)

	tmpfs := mount{target: "/old/hook"}
	at, shadow = translate(next, "hook", []mount{{source: creds, target: "/old"}, tmpfs})
	assert.Empty(t, at)
	assert.Equal(t, &tmpfs, shadow)

	at, _ = translate(next, "hook", []mount{{source: mountSource{kind: sourceVolume, name: "/srv/creds", subpath: "a"}, target: "/old"}})
	assert.Empty(t, at, "another subpath of the same volume shows other files")
	at, _ = translate(mount{target: "/new"}, "hook", []mount{{target: "/new"}})
	assert.Empty(t, at, "an anonymous volume is never shared")
}
