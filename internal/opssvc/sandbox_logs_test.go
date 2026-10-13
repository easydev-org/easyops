package opssvc

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
)

// TestSandboxLogsOwnerScoped: a foreign owner gets NotFound; the owner passes
// the ownership gate (then fails at the log read, which the fake clientset
// serves as an empty stream).
func TestSandboxLogsOwnerScoped(t *testing.T) {
	s := newTestService()
	create(t, s, "alice", "a1")
	// A foreign owner must not read alice's sandbox.
	_, err := s.SandboxLogs(context.Background(), &opsv1.SandboxLogsRequest{
		Owner: "bob", Name: "a1",
	})
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("foreign owner: code = %v, want NotFound", connect.CodeOf(err))
	}
}

// TestSandboxLogsDiagnosticsBackfill: the response always carries the pod
// diagnostics, so a Failed sandbox explains itself even when the log is empty.
func TestSandboxLogsDiagnosticsBackfill(t *testing.T) {
	s := newTestService()
	create(t, s, "alice", "a1")
	resp, err := s.SandboxLogs(context.Background(), &opsv1.SandboxLogsRequest{
		Owner: "alice", Name: "a1",
	})
	if err != nil {
		t.Fatalf("SandboxLogs: %v", err)
	}
	// The (Pending) pod phase is backfilled alongside the logs.
	if resp.GetPodPhase() == "" {
		t.Fatalf("pod_phase not backfilled: %+v", resp)
	}
}
