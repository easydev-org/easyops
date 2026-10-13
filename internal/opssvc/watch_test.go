package opssvc

import (
	"context"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/easy-platform/easyops/internal/runtimeprofiles"
	"github.com/easy-platform/easyops/internal/sandboxmgr"
	"github.com/easy-platform/easyops/internal/servicesmgr"
)

// newWatchTestService builds a Service with BOTH the sandbox and the services
// backends wired to one fake clientset.
func newWatchTestService() *Service {
	cs := fake.NewSimpleClientset()
	sbx := sandboxmgr.NewWithClientset(cs, sandboxmgr.Config{Namespace: "worker"})
	svcs := servicesmgr.NewWithClientset(cs, servicesmgr.Config{Namespace: "worker"})
	return New(Deps{Sandbox: sbx, Runtime: runtimeprofiles.DefaultSettings(), Services: svcs})
}

// seed creates one sandbox, one service and one PVC for `owner`.
func seed(t *testing.T, s *Service, owner string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.sbx.Create(ctx, sandboxmgr.Spec{Name: owner + "-sb", Image: "img:1", Creator: owner}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
	if _, err := s.services.Deploy(ctx, servicesmgr.Spec{Name: owner + "-svc", Image: "img:1", Creator: owner, ContainerPort: 8080}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	if _, err := s.services.CreatePVC(ctx, owner+"-pvc", "1Gi", "", owner, ""); err != nil {
		t.Fatalf("seed pvc: %v", err)
	}
}

// TestBuildWatchFrameSnapshotAllKinds checks the snapshot frame carries all
// three lists (owner-filtered) and flags all three as changed.
func TestBuildWatchFrameSnapshotAllKinds(t *testing.T) {
	s := newWatchTestService()
	seed(t, s, "alice")
	seed(t, s, "bob")

	frame := s.buildWatchFrame(context.Background(), "alice", true, true, true, true, false)
	if !frame.GetSnapshot() || !frame.GetSandboxesChanged() || !frame.GetServicesChanged() || !frame.GetPvcsChanged() {
		t.Fatalf("snapshot/changed flags wrong: %+v", frame)
	}
	if len(frame.GetSandboxes()) != 1 || frame.GetSandboxes()[0].GetOwner() != "alice" {
		t.Fatalf("sandboxes not owner-filtered: %+v", frame.GetSandboxes())
	}
	if len(frame.GetServices()) != 1 || frame.GetServices()[0].GetOwner() != "alice" {
		t.Fatalf("services not owner-filtered: %+v", frame.GetServices())
	}
	if len(frame.GetPvcs()) != 1 || frame.GetPvcs()[0].GetOwner() != "alice" {
		t.Fatalf("pvcs not owner-filtered: %+v", frame.GetPvcs())
	}
}

// TestBuildWatchFrameEmptyOwnerNoFilter checks an empty owner returns everything.
func TestBuildWatchFrameEmptyOwnerNoFilter(t *testing.T) {
	s := newWatchTestService()
	seed(t, s, "alice")
	seed(t, s, "bob")

	frame := s.buildWatchFrame(context.Background(), "", true, true, true, true, false)
	if len(frame.GetSandboxes()) != 2 || len(frame.GetServices()) != 2 || len(frame.GetPvcs()) != 2 {
		t.Fatalf("empty owner should not filter: sb=%d svc=%d pvc=%d",
			len(frame.GetSandboxes()), len(frame.GetServices()), len(frame.GetPvcs()))
	}
}

// TestBuildWatchFrameSelfContained checks that EVERY frame (including an
// incremental one) carries all three owner-filtered lists, while the *_changed
// bits flag only the kind(s) that actually changed this frame.
func TestBuildWatchFrameSelfContained(t *testing.T) {
	s := newWatchTestService()
	seed(t, s, "alice")

	// A services-changed frame: all three lists are still present, but only
	// ServicesChanged is set.
	frame := s.buildWatchFrame(context.Background(), "alice", false, false, true, false, false)
	if frame.GetSnapshot() || frame.GetSandboxesChanged() || !frame.GetServicesChanged() || frame.GetPvcsChanged() {
		t.Fatalf("changed flags wrong: %+v", frame)
	}
	if len(frame.GetSandboxes()) != 1 || len(frame.GetServices()) != 1 || len(frame.GetPvcs()) != 1 {
		t.Fatalf("frame must be self-contained (all three lists): sb=%d svc=%d pvc=%d",
			len(frame.GetSandboxes()), len(frame.GetServices()), len(frame.GetPvcs()))
	}

	// A sandbox-changed frame: same — all three lists present, only the sandbox
	// bit set.
	frame = s.buildWatchFrame(context.Background(), "alice", false, true, false, false, false)
	if !frame.GetSandboxesChanged() || frame.GetServicesChanged() || frame.GetPvcsChanged() {
		t.Fatalf("sandbox frame flags wrong: %+v", frame)
	}
	if len(frame.GetSandboxes()) != 1 || len(frame.GetServices()) != 1 || len(frame.GetPvcs()) != 1 {
		t.Fatalf("frame must be self-contained (all three lists): sb=%d svc=%d pvc=%d",
			len(frame.GetSandboxes()), len(frame.GetServices()), len(frame.GetPvcs()))
	}
}

// TestBuildWatchFrameNoServicesBackend checks that with the services backend
// absent the services/PVC lists stay empty and their changed bits stay false
// (the snapshot only flags the CONFIGURED kinds).
func TestBuildWatchFrameNoServicesBackend(t *testing.T) {
	s := newTestService() // sandbox backend only
	create(t, s, "alice", "a1")

	// Mirror WatchWorkspace's snapshot call: services backend absent => its bits
	// are false, not true.
	frame := s.buildWatchFrame(context.Background(), "alice", true, true, false, false, false)
	if !frame.GetSnapshot() || !frame.GetSandboxesChanged() {
		t.Fatalf("snapshot/sandbox bit wrong: %+v", frame)
	}
	if frame.GetServicesChanged() || frame.GetPvcsChanged() {
		t.Fatalf("absent services backend must not flag services/pvcs as changed: %+v", frame)
	}
	if len(frame.GetServices()) != 0 || len(frame.GetPvcs()) != 0 {
		t.Fatalf("nil services backend must yield empty services/pvcs: %+v", frame)
	}
}

// TestBuildWatchFrameNoHelmBackend checks that with the helm backend absent the
// helm list stays empty and its changed bit stays false.
func TestBuildWatchFrameNoHelmBackend(t *testing.T) {
	s := newWatchTestService() // no helm backend
	seed(t, s, "alice")

	frame := s.buildWatchFrame(context.Background(), "alice", true, true, true, true, false)
	if frame.GetHelmReleasesChanged() {
		t.Fatalf("absent helm backend must not flag helm as changed: %+v", frame)
	}
	if len(frame.GetHelmReleases()) != 0 {
		t.Fatalf("nil helm backend must yield empty helm releases: %+v", frame.GetHelmReleases())
	}
}
