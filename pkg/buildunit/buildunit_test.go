package buildunit

import (
	"testing"

	"github.com/containers/podman/v5/pkg/systemd/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const unitPath = "/etc/containers/systemd/picolet/app.build"

func TestResolve(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		unit string
		want Paths
	}{
		{
			name: "absolute File= without a context argument: its directory is the context",
			unit: "[Build]\nFile=/srv/app/Containerfile\n",
			want: Paths{Context: "/srv/app", Containerfiles: []string{"/srv/app/Containerfile"}, ContainerfilesComplete: true},
		},
		{
			// podman build tries File= in the service's default directory
			// first, which the unit does not determine, then in the context.
			name: "relative File= with a context path and no working directory",
			unit: "[Build]\nFile=Containerfile\nSetWorkingDirectory=/srv/app\n",
			want: Paths{Context: "/srv/app", Containerfiles: []string{"/srv/app/Containerfile"}},
		},
		{
			name: "relative File= in the working directory and a separate context",
			unit: "[Build]\nFile=Containerfile\nSetWorkingDirectory=/srv/app\n[Service]\nWorkingDirectory=/srv/tools\n",
			want: Paths{
				Context:                "/srv/app",
				Containerfiles:         []string{"/srv/tools/Containerfile", "/srv/app/Containerfile"},
				ContainerfilesComplete: true,
			},
		},
		{
			name: "unit-relative context path replaced by the working directory",
			unit: "[Build]\nFile=Containerfile\nSetWorkingDirectory=ctx\n[Service]\nWorkingDirectory=/srv/app\n",
			want: Paths{Context: "/srv/app", Containerfiles: []string{"/srv/app/Containerfile"}, ContainerfilesComplete: true},
		},
		{
			name: "unit-relative context path",
			unit: "[Build]\nFile=Containerfile\nSetWorkingDirectory=ctx\n",
			want: Paths{
				Context:                "/etc/containers/systemd/picolet/ctx",
				Containerfiles:         []string{"/etc/containers/systemd/picolet/Containerfile", "/etc/containers/systemd/picolet/ctx/Containerfile"},
				ContainerfilesComplete: true,
			},
		},
		{
			name: "URL context",
			unit: "[Build]\nFile=Containerfile\nSetWorkingDirectory=https://github.com/example/app.git\n",
			want: Paths{},
		},
		{
			name: "specifier in File=",
			unit: "[Build]\nFile=/srv/%i/Containerfile\nSetWorkingDirectory=file\n",
			want: Paths{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u := parser.NewUnitFile()
			require.NoError(t, u.Parse(tt.unit))
			assert.Equal(t, tt.want, Resolve(u, unitPath))
		})
	}
}
