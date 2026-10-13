package opssvc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"k8s.io/client-go/kubernetes/fake"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/gen/easyops/v1/opsv1connect"
	"github.com/easy-platform/easyops/internal/runtimeprofiles"
	"github.com/easy-platform/easyops/internal/sandboxmgr"
	"github.com/easy-platform/easyops/internal/servicesmgr"
)

// TestWatchStreamSurvivesForbiddenKind proves the stream is NOT interrupted when
// one kind's LIST fails with Forbidden (missing worker-namespace RBAC): the
// snapshot still arrives, the forbidden kind is left empty, and the other kinds
// are pushed. This locks the "single-kind list failure does not abort the
// stream" behaviour.
func TestWatchStreamSurvivesForbiddenKind(t *testing.T) {
	cs := fake.NewSimpleClientset()
	// Sandbox pod LIST is forbidden; the services/PVC lists succeed.
	forbidList(cs, "pods")
	sbx := sandboxmgr.NewWithClientset(cs, sandboxmgr.Config{Namespace: "worker"})
	svcs := servicesmgr.NewWithClientset(cs, servicesmgr.Config{Namespace: "worker"})

	// Seed a service + PVC so the OTHER kinds are non-empty (proving they are
	// still pushed while sandboxes are forbidden).
	ctx := context.Background()
	if _, err := svcs.Deploy(ctx, servicesmgr.Spec{Name: "s1", Image: "img:1", Creator: "alice", ContainerPort: 8080}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	if _, err := svcs.CreatePVC(ctx, "p1", "1Gi", "", "alice", ""); err != nil {
		t.Fatalf("seed pvc: %v", err)
	}
	svc := New(Deps{Sandbox: sbx, Runtime: runtimeprofiles.DefaultSettings(), Services: svcs})

	// Drive the real Connect handler over an in-process HTTP server.
	server := connect.NewServer()
	opsv1connect.RegisterOpsServiceHandler(server, svc)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := opsv1connect.NewOpsServiceClient(connect.NewClient(connecthttp.NewTransport(srv.Client(), srv.URL, connecthttp.WithReadMaxBytes(0))))
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.WatchWorkspace(sctx, &opsv1.WatchWorkspaceRequest{Owner: "alice"})
	if err != nil {
		t.Fatalf("watch open: %v", err)
	}
	m, err := stream.Receive()
	if err != nil {
		t.Fatalf("stream ended without a frame: %v", err)
	}
	if !m.GetSnapshot() {
		t.Fatalf("first frame is not the snapshot: %+v", m)
	}
	// The forbidden kind is empty, but the stream survived and pushed the rest.
	if n := len(m.GetSandboxes()); n != 0 {
		t.Fatalf("forbidden sandbox list must be empty, got %d", n)
	}
	if len(m.GetServices()) != 1 || len(m.GetPvcs()) != 1 {
		t.Fatalf("other kinds must still be pushed: svc=%d pvc=%d", len(m.GetServices()), len(m.GetPvcs()))
	}
	_ = stream.Close()
}
