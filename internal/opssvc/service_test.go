package opssvc

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"
	"k8s.io/client-go/kubernetes/fake"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/k8s/runtimeprofiles"
	"github.com/easy-platform/easyops/k8s/sandboxmgr"
)

func newTestService() *Service {
	sbx := sandboxmgr.NewWithClientset(fake.NewSimpleClientset(), sandboxmgr.Config{Namespace: "worker"})
	return New(Deps{Sandbox: sbx, Runtime: runtimeprofiles.DefaultSettings()})
}

func create(t *testing.T, s *Service, owner, name string) {
	t.Helper()
	_, err := s.CreateSandbox(context.Background(), &opsv1.CreateSandboxRequest{
		Owner: owner, Name: name, Image: "img:1",
	})
	if err != nil {
		t.Fatalf("create %s/%s: %v", owner, name, err)
	}
}

func TestCreateStampsOwnerAndSession(t *testing.T) {
	s := newTestService()
	resp, err := s.CreateSandbox(context.Background(), &opsv1.CreateSandboxRequest{
		Owner: "alice", Session: "org:repo:dev", Name: "sb1", Image: "img:1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.GetSandbox().GetOwner() != "alice" || resp.GetSandbox().GetSession() != "org:repo:dev" {
		t.Fatalf("owner/session not stamped: %+v", resp.GetSandbox())
	}
	if resp.GetToken() == "" {
		t.Fatal("empty token")
	}
}

func TestCreateRequiresNameAndImage(t *testing.T) {
	s := newTestService()
	_, err := s.CreateSandbox(context.Background(), &opsv1.CreateSandboxRequest{Owner: "a", Image: "img:1"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("missing name: code = %v, want InvalidArgument", connect.CodeOf(err))
	}
	_, err = s.CreateSandbox(context.Background(), &opsv1.CreateSandboxRequest{Owner: "a", Name: "x"})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("missing image: code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

func TestListFiltersByOwner(t *testing.T) {
	s := newTestService()
	create(t, s, "alice", "a1")
	create(t, s, "bob", "b1")

	al, err := s.ListSandboxes(context.Background(), &opsv1.ListSandboxesRequest{Owner: "alice"})
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(al.GetSandboxes()) != 1 || al.GetSandboxes()[0].GetName() != "a1" {
		t.Fatalf("alice sees %+v", al.GetSandboxes())
	}
	// An empty owner means "no filter" (there is NO per-owner authorization).
	all, err := s.ListSandboxes(context.Background(), &opsv1.ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all.GetSandboxes()) != 2 {
		t.Fatalf("unfiltered list = %d, want 2", len(all.GetSandboxes()))
	}
}

func TestGetAndDeleteRespectOwner(t *testing.T) {
	s := newTestService()
	create(t, s, "alice", "a1")

	// A foreign owner must not see (or delete) another owner's sandbox.
	got, err := s.GetSandbox(context.Background(), &opsv1.GetSandboxRequest{Owner: "bob", Name: "a1"})
	if err != nil {
		t.Fatalf("get bob: %v", err)
	}
	if got.GetFound() {
		t.Fatal("bob should not see alice's sandbox")
	}
	del, err := s.DeleteSandbox(context.Background(), &opsv1.DeleteSandboxRequest{Owner: "bob", Name: "a1"})
	if err != nil {
		t.Fatalf("delete bob: %v", err)
	}
	if del.GetDeleted() {
		t.Fatal("bob should not delete alice's sandbox")
	}
	// The owner can.
	del, err = s.DeleteSandbox(context.Background(), &opsv1.DeleteSandboxRequest{Owner: "alice", Name: "a1"})
	if err != nil || !del.GetDeleted() {
		t.Fatalf("alice delete: err=%v deleted=%v", err, del.GetDeleted())
	}
}

func TestResolveSandbox(t *testing.T) {
	s := newTestService()
	create(t, s, "alice", "a1")
	res, err := s.ResolveSandbox(context.Background(), &opsv1.ResolveSandboxRequest{Owner: "alice", Name: "a1"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.GetUrl() == "" || res.GetToken() == "" {
		t.Fatalf("resolve empty: %+v", res)
	}
	// A foreign owner gets NotFound, never a leak.
	_, err = s.ResolveSandbox(context.Background(), &opsv1.ResolveSandboxRequest{Owner: "bob", Name: "a1"})
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("foreign resolve: code = %v, want NotFound", connect.CodeOf(err))
	}
}

func TestUnconfiguredBackendsReturnUnavailable(t *testing.T) {
	s := newTestService() // only the sandbox backend is configured
	ctx := context.Background()
	if _, err := s.DeployService(ctx, &opsv1.DeployServiceRequest{Owner: "a", Image: "i"}); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("DeployService: code = %v, want Unavailable", connect.CodeOf(err))
	}
	if _, err := s.CreatePVC(ctx, &opsv1.CreatePVCRequest{Owner: "a", Name: "p"}); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("CreatePVC: code = %v, want Unavailable", connect.CodeOf(err))
	}
	if _, err := s.BuildImage(ctx, &opsv1.BuildImageRequest{Owner: "a", Repo: "o/i", Tag: "t"}); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("BuildImage: code = %v, want Unavailable", connect.CodeOf(err))
	}
}
