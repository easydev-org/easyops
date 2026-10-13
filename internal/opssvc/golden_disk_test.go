package opssvc

import (
	"context"
	"testing"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
)

// TestGoldenDiskNoServicesBackend: golden_disk=true with the services backend
// NIL must NOT panic (regression: a nil s.services deref) — the sandbox is still
// created, just without the shared cache PVC.
func TestGoldenDiskNoServicesBackend(t *testing.T) {
	s := newTestService() // no services backend
	resp, err := s.CreateSandbox(context.Background(), &opsv1.CreateSandboxRequest{
		Owner: "alice", Name: "vm", Image: "img:1", GoldenDisk: true,
	})
	if err != nil {
		t.Fatalf("create with golden_disk and no services backend: %v", err)
	}
	if resp.GetSandbox().GetName() != "vm" {
		t.Fatalf("sandbox not created: %+v", resp)
	}
}
