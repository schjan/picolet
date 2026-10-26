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

// unitMounts reads each mount line's target as Quadlet and Podman do, in
// every spelling the guard relies on, and what a fresh start finds there.
func TestUnitMounts(t *testing.T) {
	t.Parallel()
	data := []mount{{target: "/data"}}
	empty := []mount{{target: "/data", empty: true}}
	tests := []struct {
		line        string
		want        []mount
		volumesFrom []string
	}{
		{line: "Volume=/data", want: empty},
		{line: "Volume=:/data", want: empty},
		{line: "Volume=/srv/s:/data/", want: data},
		{line: "Volume=creds.volume:/data", want: []mount{{target: "/data", volumeUnit: "creds.volume"}}},
		{line: "Mount=type=volume,source=creds.volume,destination=/data", want: []mount{{target: "/data", volumeUnit: "creds.volume"}}},
		{line: "Mount=target=/data", want: empty},
		{line: `Mount="type=bind,src=/srv/s,dst=/data,ro"`, want: data},
		{line: "Mount=type=tmpfs,destination=/data", want: empty},
		{line: "Mount=type=image,source=creds.build,destination=/data", want: []mount{{target: "/data", image: true}}},
		{line: "Tmpfs=/data:size=1m", want: empty},
		{line: "VolatileTmp=true", want: []mount{{target: "/tmp", empty: true}}},
		{line: "Secret=tok", want: []mount{{target: "/run/secrets/tok", secret: "tok"}}},
		{line: "Secret=target=/data/tok,source=tok,mode=0400", want: []mount{{target: "/data/tok", secret: "tok"}}},
		{line: "Secret=tok,type=env"},
		{line: "PodmanArgs=--volume /srv/s:/data", want: data},
		{line: "PodmanArgs=--volume=/data", want: empty},
		{line: "PodmanArgs=-dv=/srv/s:/data", want: data},
		{line: "PodmanArgs=--security-opt apparmor=unconfined -v/srv/s:/data", want: data},
		{line: "PodmanArgs=--mount=type=bind,src=/srv/s,dst=/data", want: data},
		{line: "PodmanArgs=--mount type=image,src=creds,dst=/data", want: []mount{{target: "/data", image: true}}},
		{line: "PodmanArgs=--volume creds.volume:/data", want: data},
		{line: "PodmanArgs=--tmpfs /data", want: empty},
		{line: "PodmanArgs=--secret tok,target=/data/tok", want: []mount{{target: "/data/tok", secret: "tok"}}},
		{line: "PodmanArgs=-e v=1 -w /data"},
		{line: "PodmanArgs=--volume"},
		{line: "PodmanArgs=--volumes-from=other:ro", volumesFrom: []string{"--volumes-from other:ro"}},
	}
	for _, tt := range tests {
		mounts, volumesFrom := unitMounts(parseContainerUnit(t, tt.line+"\n"))
		assert.Equal(t, tt.want, mounts, tt.line)
		assert.Equal(t, tt.volumesFrom, volumesFrom, tt.line)
	}
}

// mayHold returns the mounts at the deepest target holding a path, then
// every mount at a target the guard cannot place.
func TestMayHold(t *testing.T) {
	t.Parallel()
	a := mount{target: "/data"}
	b := mount{target: "/data", empty: true}
	sub := mount{target: "/data/sub"}
	unplaced := mount{target: "%h/x"}
	mounts := []mount{a, sub, unplaced, b}

	assert.Equal(t, []mount{a, b, unplaced}, mayHold(mounts, "/data/x"))
	assert.Equal(t, []mount{sub, unplaced}, mayHold(mounts, "/data/sub/x/"))
	assert.Equal(t, []mount{unplaced}, mayHold(mounts, "/database"))
}

// unitSecrets lists every secret a unit uses, mounted or not, and where.
func TestUnitSecrets(t *testing.T) {
	t.Parallel()
	unit := parseContainerUnit(t, "Secret=a\nSecret=source=b,type=env,target=B\nPodmanArgs=--secret=c,type=env -dv /x --secret d\n")
	assert.Equal(t, []unitSecret{
		{name: "a", decl: "Secret=a"},
		{name: "b", decl: "Secret=source=b,type=env,target=B"},
		{name: "c", decl: "PodmanArgs=--secret c,type=env"},
		{name: "d", decl: "PodmanArgs=--secret d"},
	}, unitSecrets(unit))
}
