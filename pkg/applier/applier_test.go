package applier

import (
	"context"
	"fmt"
	"testing"

	"github.com/schjan/picolet/pkg/reconciler"
)

// mockSystemd records calls for testing.
type mockSystemd struct {
	reloads   int
	started   []string
	restarted []string
	states    map[string]string
	failStart map[string]bool
}

func newMockSystemd() *mockSystemd {
	return &mockSystemd{
		states:    make(map[string]string),
		failStart: make(map[string]bool),
	}
}

func (m *mockSystemd) DaemonReload(context.Context) error {
	m.reloads++
	return nil
}

func (m *mockSystemd) StartUnit(_ context.Context, name string) error {
	if m.failStart[name] {
		return fmt.Errorf("start %s: mock failure", name)
	}
	m.started = append(m.started, name)
	return nil
}

func (m *mockSystemd) RestartUnit(_ context.Context, name string) error {
	m.restarted = append(m.restarted, name)
	return nil
}

func (m *mockSystemd) GetUnitState(_ context.Context, name string) (string, error) {
	s, ok := m.states[name]
	if !ok {
		return "inactive", nil
	}
	return s, nil
}

//nolint:contextcheck // test mock uses context.Background()
func (m *mockSystemd) IsActive(_ context.Context, name string) (bool, error) {
	s, _ := m.GetUnitState(context.Background(), name)
	return s == "active", nil
}

// mockPodman records secret operations.
type mockPodman struct {
	secrets   map[string][]byte
	created   []string
	healthOK  map[string]bool
	podStates map[string]string
}

func newMockPodman() *mockPodman {
	return &mockPodman{
		secrets:   make(map[string][]byte),
		healthOK:  make(map[string]bool),
		podStates: make(map[string]string),
	}
}

func (m *mockPodman) SecretExists(_ context.Context, name string) (bool, error) {
	_, ok := m.secrets[name]
	return ok, nil
}

func (m *mockPodman) SecretCreate(_ context.Context, name string, data []byte, _ bool) error {
	m.secrets[name] = data
	m.created = append(m.created, name)
	return nil
}

func (m *mockPodman) RunHealthcheck(_ context.Context, container string) (bool, error) {
	return m.healthOK[container], nil
}

func (m *mockPodman) GetPodState(_ context.Context, pod string) (string, error) {
	s, ok := m.podStates[pod]
	if !ok {
		return "unknown", nil
	}
	return s, nil
}

func TestApplyPhaseOrdering(t *testing.T) {
	sys := newMockSystemd()
	pod := newMockPodman()
	fw := newMemFileWriter()
	a := New(sys, pod, fw, false)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			// Out of order deliberately
			{DestPath: "/etc/containers/systemd/app.container", Category: "container", Action: reconciler.ActionCreate, NewContent: "[Container]\nImage=foo"},
			{DestPath: "/etc/containers/systemd/data.volume", Category: "volume", Action: reconciler.ActionCreate, NewContent: "[Volume]"},
			{DestPath: "secret:my_secret", Category: "secret", Action: reconciler.ActionCreate, NewContent: "token=abc"},
			{DestPath: "/etc/containers/systemd/net.network", Category: "network", Action: reconciler.ActionCreate, NewContent: "[Network]"},
			{DestPath: "/var/lib/picolet/manifests/app/deploy.yml", Category: "manifest", Action: reconciler.ActionCreate, NewContent: "apiVersion: v1"},
			{DestPath: "/etc/containers/systemd/app.kube", Category: "kube", Action: reconciler.ActionCreate, NewContent: "[Kube]\nYaml=foo"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionCreate: 6},
	}

	result, err := a.Apply(context.Background(), cs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied != 6 {
		t.Errorf("Applied = %d, want 6", result.Applied)
	}

	// Verify network was written (file category)
	if _, ok := fw.written["/etc/containers/systemd/net.network"]; !ok {
		t.Error("network file not written")
	}
	// Verify volume was written
	if _, ok := fw.written["/etc/containers/systemd/data.volume"]; !ok {
		t.Error("volume file not written")
	}
	// Verify secret was created via podman
	if _, ok := pod.secrets["my_secret"]; !ok {
		t.Error("secret not created via podman")
	}
	// Verify daemon-reload was called
	if sys.reloads != 1 {
		t.Errorf("reloads = %d, want 1", sys.reloads)
	}
}

func TestApplyDryRun(t *testing.T) {
	sys := newMockSystemd()
	pod := newMockPodman()
	fw := newMemFileWriter()
	a := New(sys, pod, fw, true)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/foo.container", Category: "container", Action: reconciler.ActionCreate, NewContent: "content"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionCreate: 1},
	}

	result, err := a.Apply(context.Background(), cs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied != 1 {
		t.Errorf("Applied = %d, want 1", result.Applied)
	}
	// No actual writes in dry-run
	if len(fw.written) != 0 {
		t.Errorf("files written in dry-run: %d", len(fw.written))
	}
	if sys.reloads != 0 {
		t.Errorf("daemon-reload in dry-run: %d", sys.reloads)
	}
}

func TestApplyNoop(t *testing.T) {
	sys := newMockSystemd()
	pod := newMockPodman()
	fw := newMemFileWriter()
	a := New(sys, pod, fw, false)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/foo.container", Category: "container", Action: reconciler.ActionNoop},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionNoop: 1},
	}

	result, err := a.Apply(context.Background(), cs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied != 0 {
		t.Errorf("Applied = %d, want 0 (noop)", result.Applied)
	}
}

func TestApplyDelete(t *testing.T) {
	sys := newMockSystemd()
	pod := newMockPodman()
	fw := newMemFileWriter()
	a := New(sys, pod, fw, false)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/old.container", Category: "container", Action: reconciler.ActionDelete},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionDelete: 1},
	}

	result, err := a.Apply(context.Background(), cs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied != 1 {
		t.Errorf("Applied = %d, want 1", result.Applied)
	}
	if len(fw.removed) != 1 || fw.removed[0] != "/etc/containers/systemd/old.container" {
		t.Errorf("removed = %v, want [/etc/containers/systemd/old.container]", fw.removed)
	}
}

func TestApplySelfRestart(t *testing.T) {
	sys := newMockSystemd()
	pod := newMockPodman()
	fw := newMemFileWriter()
	a := New(sys, pod, fw, false)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "/etc/containers/systemd/picolet.container", Category: "container", Action: reconciler.ActionUpdate, NewContent: "updated"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	}

	result, err := a.Apply(context.Background(), cs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.NeedsSelfRestart {
		t.Error("expected NeedsSelfRestart=true")
	}
}

func TestApplySecretReplace(t *testing.T) {
	sys := newMockSystemd()
	pod := newMockPodman()
	pod.secrets["cfg"] = []byte("old")
	fw := newMemFileWriter()
	a := New(sys, pod, fw, false)

	cs := &reconciler.Changeset{
		Changes: []reconciler.Change{
			{DestPath: "secret:cfg", Category: "secret", Action: reconciler.ActionUpdate, NewContent: "new-data"},
		},
		Summary: map[reconciler.Action]int{reconciler.ActionUpdate: 1},
	}

	result, err := a.Apply(context.Background(), cs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied != 1 {
		t.Errorf("Applied = %d, want 1", result.Applied)
	}
	if string(pod.secrets["cfg"]) != "new-data" {
		t.Errorf("secret content = %q, want new-data", pod.secrets["cfg"])
	}
}
