package applier_test

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	appliermocks "github.com/schjan/picolet/mocks/applier"
	"github.com/schjan/picolet/pkg/applier"
	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/reconciler"
)

const testQuadletDir = "/etc/containers/systemd/picolet/"

func podChange(action reconciler.Action) reconciler.Change {
	return reconciler.Change{
		DestPath: testQuadletDir + "web.pod", Category: config.CategoryPod, Action: action,
		NewContent: "[Pod]\n", ServiceName: "web-pod.service",
	}
}

func containerChange(name, content string, action reconciler.Action) reconciler.Change {
	return reconciler.Change{
		DestPath: testQuadletDir + name + ".container", Category: config.CategoryContainer, Action: action,
		NewContent: content, ServiceName: name + ".service",
	}
}

func TestApplyWritesPodBeforeItsContainers(t *testing.T) {
	t.Parallel()
	sys := appliermocks.NewMockSystemdManager(t)
	sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
	sys.EXPECT().RestartUnit(mock.Anything, mock.Anything).Return(nil)
	fw := newMemFileWriter()
	a := applier.New(sys, appliermocks.NewMockPodmanClient(t), fw, false, nil)

	_, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: []reconciler.Change{
		containerChange("api", "[Container]\nImage=api\nPod=web.pod\n", reconciler.ActionCreate),
		{DestPath: testQuadletDir + "stack.kube", Category: config.CategoryKube, Action: reconciler.ActionCreate, NewContent: "[Kube]\nYaml=x.yml\n", ServiceName: "stack.service"},
		podChange(reconciler.ActionCreate),
	}})
	require.NoError(t, err)

	pod := slices.Index(fw.writeOrder, testQuadletDir+"web.pod")
	require.GreaterOrEqual(t, pod, 0, "pod file not written")
	assert.Less(t, pod, slices.Index(fw.writeOrder, testQuadletDir+"api.container"))
	assert.Less(t, pod, slices.Index(fw.writeOrder, testQuadletDir+"stack.kube"))
}

// A restarting pod stops its members (BindsTo=) and starts them again (Wants=),
// so a member changed alongside it must not be restarted a second time.
func TestApplyPodRestartCoversMemberContainers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		changes       []reconciler.Change
		wantRestarted []string
	}{
		{
			name: "members changed with their pod restart only via the pod",
			changes: []reconciler.Change{
				podChange(reconciler.ActionUpdate),
				containerChange("api", "[Container]\nImage=api\nPod=web.pod\n", reconciler.ActionUpdate),
				containerChange("proxy", "[Container]\nImage=nginx\nPod=web.pod\n", reconciler.ActionCreate),
				containerChange("solo", "[Container]\nImage=solo\n", reconciler.ActionUpdate),
			},
			wantRestarted: []string{"solo.service", "web-pod.service"},
		},
		{
			name: "member the pod does not start keeps its own restart",
			changes: []reconciler.Change{
				podChange(reconciler.ActionUpdate),
				containerChange("api", "[Container]\nImage=api\nPod=web.pod\nStartWithPod=false\n", reconciler.ActionUpdate),
			},
			wantRestarted: []string{"api.service", "web-pod.service"},
		},
		{
			name: "member changed without its pod is restarted",
			changes: []reconciler.Change{
				containerChange("api", "[Container]\nImage=api\nPod=web.pod\n", reconciler.ActionUpdate),
			},
			wantRestarted: []string{"api.service"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sys := appliermocks.NewMockSystemdManager(t)
			sys.EXPECT().DaemonReload(mock.Anything).Return(nil)
			for _, unit := range tt.wantRestarted {
				sys.EXPECT().RestartUnit(mock.Anything, unit).Return(nil).Once()
			}
			a := applier.New(sys, appliermocks.NewMockPodmanClient(t), newMemFileWriter(), false, nil)

			result, err := a.Apply(context.Background(), &reconciler.Changeset{Changes: tt.changes})
			require.NoError(t, err)
			assert.Equal(t, tt.wantRestarted, result.RestartedUnits)
		})
	}
}
