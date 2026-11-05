//go:build integration

package rpce2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	easyopsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/gen/easyops/v1/opsv1connect"
)

// TestRPC_AuthOnTheWire is the property the whole surface depends on: every call
// without a valid token is Unauthenticated, and this must hold for a STREAMING
// call too (a unary-only test would miss the streaming interceptor path).
func TestRPC_AuthOnTheWire(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Unauthenticated: no token, a wrong token.
	for name, c := range map[string]opsv1connect.OpsServiceClient{"anon": h.Anon, "bad": h.Bad} {
		_, err := c.ListJobs(ctx, &easyopsv1.ListJobsRequest{})
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("%s: code = %v, want Unauthenticated (err=%v)", name, connect.CodeOf(err), err)
		}
	}

	// The token-bearing client succeeds, proving the harness is otherwise sound.
	if _, err := h.Client.ListJobs(ctx, &easyopsv1.ListJobsRequest{}); err != nil {
		t.Fatalf("authenticated ListJobs: %v", err)
	}

	// A STREAM: an anonymous caller must be rejected before any frame arrives.
	stream, err := h.Anon.WatchWorkspace(ctx, &easyopsv1.WatchWorkspaceRequest{})
	if err == nil {
		defer stream.Close()
		_, err = stream.Receive()
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous stream: code = %v, want Unauthenticated (err=%v)", connect.CodeOf(err), err)
	}
}

// TestRPC_SandboxSurface covers all 7 sandbox RPCs end to end.
func TestRPC_SandboxSurface(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	name := fmt.Sprintf("rpc-sbx-%d", time.Now().UnixNano())
	owner := "alice"
	img := fixture(t, "worker:1")

	create, err := h.Client.CreateSandbox(ctx, &easyopsv1.CreateSandboxRequest{
		Owner: owner, Session: "s1", Name: name, Image: img,
		// Wait (bounded) so the RPC returns a ready sandbox, as the gateway does.
		ReadyTimeoutSeconds: 180,
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if create.GetSandbox().GetName() != name || create.GetToken() == "" {
		t.Fatalf("CreateSandbox = %+v", create)
	}
	defer func() {
		_, _ = h.Client.DeleteSandbox(context.Background(), &easyopsv1.DeleteSandboxRequest{Owner: owner, Name: name})
	}()
	if !create.GetSandbox().GetReady() {
		t.Errorf("a sandbox created with ready_timeout should come back ready: %+v", create.GetSandbox())
	}

	// Get.
	get, err := h.Client.GetSandbox(ctx, &easyopsv1.GetSandboxRequest{Owner: owner, Name: name})
	if err != nil || !get.GetFound() {
		t.Fatalf("GetSandbox: found=%v err=%v", get.GetFound(), err)
	}

	// Resolve returns url + token.
	res, err := h.Client.ResolveSandbox(ctx, &easyopsv1.ResolveSandboxRequest{Owner: owner, Name: name})
	if err != nil {
		t.Fatalf("ResolveSandbox: %v", err)
	}
	if res.GetUrl() == "" || res.GetToken() == "" {
		t.Errorf("ResolveSandbox = %+v", res)
	}
	if res.GetToken() != create.GetToken() {
		t.Error("ResolveSandbox must return the token CreateSandbox minted")
	}

	// List, owner-scoped.
	list, err := h.Client.ListSandboxes(ctx, &easyopsv1.ListSandboxesRequest{Owner: owner})
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	var seen bool
	for _, s := range list.GetSandboxes() {
		if s.GetName() == name {
			seen = true
		}
	}
	if !seen {
		t.Errorf("ListSandboxes does not include %q", name)
	}

	// The OWNER filter must hide it from another owner.
	other, err := h.Client.ListSandboxes(ctx, &easyopsv1.ListSandboxesRequest{Owner: "bob"})
	if err != nil {
		t.Fatalf("ListSandboxes(bob): %v", err)
	}
	for _, s := range other.GetSandboxes() {
		if s.GetName() == name {
			t.Errorf("another owner must not see the sandbox")
		}
	}

	// Logs.
	logs, err := h.Client.SandboxLogs(ctx, &easyopsv1.SandboxLogsRequest{Owner: owner, Name: name, TailLines: 20})
	if err != nil {
		t.Fatalf("SandboxLogs: %v", err)
	}
	if len(logs.GetLines()) == 0 {
		t.Errorf("SandboxLogs returned no lines")
	}

	// WatchSandboxLogs: receive at least one frame, then stop.
	stream, err := h.Client.WatchSandboxLogs(ctx, &easyopsv1.WatchSandboxLogsRequest{Owner: owner, Name: name})
	if err != nil {
		t.Fatalf("WatchSandboxLogs: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Errorf("WatchSandboxLogs first frame: %v", err)
	}
	_ = stream.Close()

	// Delete, then it is gone.
	if _, err := h.Client.DeleteSandbox(ctx, &easyopsv1.DeleteSandboxRequest{Owner: owner, Name: name}); err != nil {
		t.Fatalf("DeleteSandbox: %v", err)
	}
	gone, err := h.Client.GetSandbox(ctx, &easyopsv1.GetSandboxRequest{Owner: owner, Name: name})
	if err != nil {
		t.Fatalf("GetSandbox after delete: %v", err)
	}
	if gone.GetFound() {
		t.Error("the sandbox still exists after DeleteSandbox")
	}
}

// TestRPC_OwnerIsolation covers the cross-tenant contract at the WIRE level: an
// object owned by A is not-found for B (existence is never leaked).
func TestRPC_OwnerIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	name := fmt.Sprintf("rpc-iso-%d", time.Now().UnixNano())

	if _, err := h.Client.CreateSandbox(ctx, &easyopsv1.CreateSandboxRequest{
		Owner: "alice", Name: name, Image: fixture(t, "worker:1"),
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() {
		_, _ = h.Client.DeleteSandbox(context.Background(), &easyopsv1.DeleteSandboxRequest{Owner: "alice", Name: name})
	}()

	// Another owner's Get is found=false (not a 403, not a leak).
	got, err := h.Client.GetSandbox(ctx, &easyopsv1.GetSandboxRequest{Owner: "bob", Name: name})
	if err != nil {
		t.Fatalf("GetSandbox(bob): %v", err)
	}
	if got.GetFound() {
		t.Error("another owner must not see the sandbox")
	}
	// Delete by another owner reports deleted=false (the documented contract:
	// the object is simply not theirs, and existence never leaks as an error).
	if got, err := h.Client.DeleteSandbox(ctx, &easyopsv1.DeleteSandboxRequest{Owner: "bob", Name: name}); err != nil {
		t.Errorf("cross-owner delete: %v", err)
	} else if got.GetDeleted() {
		t.Error("another owner must not be able to delete the sandbox")
	}
	// And it is still there for its real owner.
	if got, err := h.Client.GetSandbox(ctx, &easyopsv1.GetSandboxRequest{Owner: "alice", Name: name}); err != nil || !got.GetFound() {
		t.Errorf("the sandbox must survive a cross-owner delete (found=%v err=%v)", got.GetFound(), err)
	}
}

// TestRPC_ServiceSurface covers all 12 service RPCs.
func TestRPC_ServiceSurface(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	name := fmt.Sprintf("rpc-svc-%d", time.Now().UnixNano())
	owner := "alice"
	img := fixture(t, "web:1")

	dep, err := h.Client.DeployService(ctx, &easyopsv1.DeployServiceRequest{
		Owner: owner, Session: "sess", Name: name, Image: img,
		ContainerPort: 80, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("DeployService: %v", err)
	}
	if dep.GetService().GetName() != name {
		t.Fatalf("DeployService = %+v", dep.GetService())
	}
	defer func() {
		_, _ = h.Client.DeleteService(context.Background(), &easyopsv1.DeleteServiceRequest{Owner: owner, Name: name})
	}()

	if _, err := h.Client.GetService(ctx, &easyopsv1.GetServiceRequest{Owner: owner, Name: name}); err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if _, err := h.Client.ListServices(ctx, &easyopsv1.ListServicesRequest{Owner: owner}); err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if _, err := h.Client.GetServiceManifest(ctx, &easyopsv1.GetServiceManifestRequest{Owner: owner, Name: name}); err != nil {
		t.Fatalf("GetServiceManifest: %v", err)
	}

	// Scale.
	if _, err := h.Client.ScaleService(ctx, &easyopsv1.ScaleServiceRequest{Owner: owner, Name: name, Replicas: 2}); err != nil {
		t.Fatalf("ScaleService: %v", err)
	}
	// Pause / Resume.
	if _, err := h.Client.PauseService(ctx, &easyopsv1.PauseServiceRequest{Owner: owner, Name: name}); err != nil {
		t.Fatalf("PauseService: %v", err)
	}
	if _, err := h.Client.ResumeService(ctx, &easyopsv1.ResumeServiceRequest{Owner: owner, Name: name}); err != nil {
		t.Fatalf("ResumeService: %v", err)
	}

	// ApplyServiceManifest (round-trip the manifest we read).
	m, err := h.Client.GetServiceManifest(ctx, &easyopsv1.GetServiceManifestRequest{Owner: owner, Name: name})
	if err != nil {
		t.Fatalf("GetServiceManifest: %v", err)
	}
	if _, err := h.Client.ApplyServiceManifest(ctx, &easyopsv1.ApplyServiceManifestRequest{
		Owner: owner, Name: name, Yaml: m.GetYaml(),
	}); err != nil {
		t.Fatalf("ApplyServiceManifest: %v", err)
	}

	// Logs: the RPC answers even before the pod logs (possibly empty).
	if _, err := h.Client.ServiceLogs(ctx, &easyopsv1.ServiceLogsRequest{Owner: owner, Name: name, TailLines: 10}); err != nil {
		t.Fatalf("ServiceLogs: %v", err)
	}

	// WatchServiceLogs: the stream must at least open and be closeable.
	ws, err := h.Client.WatchServiceLogs(ctx, &easyopsv1.WatchServiceLogsRequest{Owner: owner, Name: name})
	if err != nil {
		t.Fatalf("WatchServiceLogs: %v", err)
	}
	_ = ws.Close()

	// Rollback (revision 0 = previous). The first deploy is revision 1, so a
	// rollback may legitimately report "no previous revision"; accept either a
	// successful rollback or the documented error, but not a panic/crash.
	if _, err := h.Client.RollbackService(ctx, &easyopsv1.RollbackServiceRequest{Owner: owner, Name: name}); err != nil {
		if connect.CodeOf(err) == connect.CodeInternal {
			t.Errorf("RollbackService must not fail with Internal: %v", err)
		}
	}

	// Delete.
	if _, err := h.Client.DeleteService(ctx, &easyopsv1.DeleteServiceRequest{Owner: owner, Name: name}); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if got, err := h.Client.GetService(ctx, &easyopsv1.GetServiceRequest{Owner: owner, Name: name}); err != nil {
		t.Errorf("GetService after delete: %v", err)
	} else if got.GetFound() {
		t.Error("the service still exists after DeleteService")
	}
}

// TestRPC_PVCSurface covers all 4 PVC RPCs.
func TestRPC_PVCSurface(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	name := fmt.Sprintf("rpc-pvc-%d", time.Now().UnixNano())
	owner := "alice"

	if _, err := h.Client.CreatePVC(ctx, &easyopsv1.CreatePVCRequest{
		Owner: owner, Name: name, Size: "100Mi", Namespace: "team-a",
	}); err != nil {
		t.Fatalf("CreatePVC: %v", err)
	}
	defer func() {
		_, _ = h.Client.DeletePVC(context.Background(), &easyopsv1.DeletePVCRequest{Owner: owner, Name: name})
	}()

	if _, err := h.Client.GetPVC(ctx, &easyopsv1.GetPVCRequest{Owner: owner, Name: name}); err != nil {
		t.Fatalf("GetPVC: %v", err)
	}
	list, err := h.Client.ListPVCs(ctx, &easyopsv1.ListPVCsRequest{Owner: owner})
	if err != nil {
		t.Fatalf("ListPVCs: %v", err)
	}
	var seen bool
	for _, p := range list.GetPvcs() {
		if p.GetName() == name {
			seen = true
		}
	}
	if !seen {
		t.Errorf("ListPVCs does not include %q", name)
	}
	if _, err := h.Client.DeletePVC(ctx, &easyopsv1.DeletePVCRequest{Owner: owner, Name: name}); err != nil {
		t.Fatalf("DeletePVC: %v", err)
	}
	if got, err := h.Client.GetPVC(ctx, &easyopsv1.GetPVCRequest{Owner: owner, Name: name}); err != nil {
		t.Errorf("GetPVC after delete: %v", err)
	} else if got.GetFound() {
		t.Error("the PVC still exists after DeletePVC")
	}
}

// TestRPC_ConfigAndSecretSurface covers all 8 configmap/secret RPCs.
func TestRPC_ConfigAndSecretSurface(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owner := "alice"
	cmName := fmt.Sprintf("rpc-cm-%d", time.Now().UnixNano())
	sName := fmt.Sprintf("rpc-sec-%d", time.Now().UnixNano())

	// --- ConfigMap ---
	if _, err := h.Client.PutConfigMap(ctx, &easyopsv1.PutConfigMapRequest{
		Owner: owner, Name: cmName, Data: map[string]string{"k": "v"},
	}); err != nil {
		t.Fatalf("PutConfigMap: %v", err)
	}
	defer func() {
		_, _ = h.Client.DeleteConfigMap(context.Background(), &easyopsv1.DeleteConfigMapRequest{Owner: owner, Name: cmName})
	}()
	cm, err := h.Client.GetConfigMap(ctx, &easyopsv1.GetConfigMapRequest{Owner: owner, Name: cmName})
	if err != nil || !cm.GetFound() {
		t.Fatalf("GetConfigMap: found=%v err=%v", cm.GetFound(), err)
	}
	if cm.GetConfigmap().GetData()["k"] != "v" {
		t.Errorf("ConfigMap data = %v", cm.GetConfigmap().GetData())
	}
	if _, err := h.Client.ListConfigMaps(ctx, &easyopsv1.ListConfigMapsRequest{Owner: owner}); err != nil {
		t.Fatalf("ListConfigMaps: %v", err)
	}
	if _, err := h.Client.DeleteConfigMap(ctx, &easyopsv1.DeleteConfigMapRequest{Owner: owner, Name: cmName}); err != nil {
		t.Fatalf("DeleteConfigMap: %v", err)
	}

	// --- Secret ---
	if _, err := h.Client.PutSecret(ctx, &easyopsv1.PutSecretRequest{
		Owner: owner, Name: sName, Data: map[string]string{"password": "hunter2"},
	}); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	defer func() {
		_, _ = h.Client.DeleteSecret(context.Background(), &easyopsv1.DeleteSecretRequest{Owner: owner, Name: sName})
	}()
	sec, err := h.Client.GetSecret(ctx, &easyopsv1.GetSecretRequest{Owner: owner, Name: sName})
	if err != nil || !sec.GetFound() {
		t.Fatalf("GetSecret: found=%v err=%v", sec.GetFound(), err)
	}
	if _, err := h.Client.ListSecrets(ctx, &easyopsv1.ListSecretsRequest{Owner: owner}); err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if _, err := h.Client.DeleteSecret(ctx, &easyopsv1.DeleteSecretRequest{Owner: owner, Name: sName}); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
}

// TestRPC_JobSurface covers all 6 job RPCs.
func TestRPC_JobSurface(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	name := fmt.Sprintf("rpc-job-%d", time.Now().UnixNano())
	owner := "alice"

	created, err := h.Client.CreateJob(ctx, &easyopsv1.CreateJobRequest{
		// namespace is the TENANT label, not a k8s namespace: the server pins the
		// latter from its own config (otherwise a caller could run a container in
		// any namespace).
		Owner: owner, Name: name, Namespace: "team-a",
		Image:   fixture(t, "worker:1"),
		Command: []string{"/bin/sh"}, Args: []string{"-c", "echo hello-from-rpc"},
		ActiveDeadlineSeconds: 120,
		// Opt into the bounded inline wait so the test does not have to poll.
		Wait: true, WaitSeconds: 120,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if created.GetJob().GetName() == "" {
		t.Fatal("CreateJob returned no name")
	}
	defer func() {
		_, _ = h.Client.DeleteJob(context.Background(), &easyopsv1.DeleteJobRequest{Owner: owner, Name: name})
	}()

	// Wait for it to finish, then assert the log came back through the wire.
	deadline := time.Now().Add(2 * time.Minute)
	var done bool
	for time.Now().Before(deadline) {
		g, err := h.Client.GetJob(ctx, &easyopsv1.GetJobRequest{Owner: owner, Name: name})
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if g.GetJob().GetDone() {
			done = true
			break
		}
		time.Sleep(time.Second)
	}
	if !done {
		t.Fatal("the job did not finish")
	}
	logs, err := h.Client.JobLogs(ctx, &easyopsv1.JobLogsRequest{Owner: owner, Name: name, TailLines: 50})
	if err != nil {
		t.Fatalf("JobLogs: %v", err)
	}
	if !strings.Contains(logs.GetLog(), "hello-from-rpc") {
		t.Errorf("JobLogs = %q", logs.GetLog())
	}
	if _, err := h.Client.ListJobs(ctx, &easyopsv1.ListJobsRequest{Owner: owner}); err != nil {
		t.Fatalf("ListJobs: %v", err)
	}

	// WatchJobLogs opens and closes cleanly.
	ws, err := h.Client.WatchJobLogs(ctx, &easyopsv1.WatchJobLogsRequest{Owner: owner, Name: name, TailLines: 10})
	if err != nil {
		t.Fatalf("WatchJobLogs: %v", err)
	}
	_ = ws.Close()

	if _, err := h.Client.DeleteJob(ctx, &easyopsv1.DeleteJobRequest{Owner: owner, Name: name}); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	// GetJob maps a missing job to NotFound (it does not use the found flag).
	if _, err := h.Client.GetJob(ctx, &easyopsv1.GetJobRequest{Owner: owner, Name: name}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("GetJob after delete code = %v, want NotFound (err=%v)", connect.CodeOf(err), err)
	}
}

// TestRPC_BuildSurface covers the 4 asynchronous-build RPCs. They are
// deliberately usable WITHOUT a buildkitd: CreateBuild validates its inputs and
// reports the backend as unavailable, and list/get/cancel keep working over the
// registry.
func TestRPC_BuildSurface(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// ListBuilds must work with no builder configured.
	if _, err := h.Client.ListBuilds(ctx, &easyopsv1.ListBuildsRequest{}); err != nil {
		t.Fatalf("ListBuilds: %v", err)
	}

	// CreateBuild without an archive must be InvalidArgument, not a crash.
	_, err := h.Client.CreateBuild(ctx, &easyopsv1.CreateBuildRequest{Repo: "x", Tag: "y"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("CreateBuild(no archive) code = %v, want InvalidArgument", connect.CodeOf(err))
	}

	// With a valid request but no builder, it must be Unavailable (documented),
	// NOT Internal.
	_, err = h.Client.CreateBuild(ctx, &easyopsv1.CreateBuildRequest{
		Archive: []byte("x"), Repo: "r", Tag: "t",
	})
	if code := connect.CodeOf(err); code != connect.CodeUnavailable && code != connect.CodeInvalidArgument {
		t.Errorf("CreateBuild(no builder) code = %v, want Unavailable/InvalidArgument (err=%v)", code, err)
	}

	// Get/Cancel of an unknown build report found=false (the documented
	// contract), and must not panic.
	if got, err := h.Client.GetBuild(ctx, &easyopsv1.GetBuildRequest{Id: "nope"}); err != nil {
		t.Errorf("GetBuild(nope): %v", err)
	} else if got.GetFound() {
		t.Error("an unknown build must not be found")
	}
	if got, err := h.Client.CancelBuild(ctx, &easyopsv1.CancelBuildRequest{Id: "nope"}); err != nil {
		t.Errorf("CancelBuild(nope): %v", err)
	} else if got.GetFound() {
		t.Error("an unknown build must not be found")
	}
}

// TestRPC_ImageSurface covers the image RPCs' CONTRACT with no buildkitd
// configured: every one of them must answer Unavailable, never Internal and never
// hang. (A real build is covered once buildkitd is added to the harness.)
func TestRPC_ImageSurface(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	archive := []byte("not-a-tarball")

	cases := []struct {
		name string
		call func() error
	}{
		{"BuildImage", func() error {
			_, err := h.Client.BuildImage(ctx, &easyopsv1.BuildImageRequest{Archive: archive, Repo: "r", Tag: "t"})
			return err
		}},
		{"BuildFiles", func() error {
			_, err := h.Client.BuildFiles(ctx, &easyopsv1.BuildImageRequest{Archive: archive, Repo: "r", Tag: "t", Output: "files"})
			return err
		}},
		{"ImportImage", func() error {
			_, err := h.Client.ImportImage(ctx, &easyopsv1.ImportImageRequest{Source: "alpine:3.20", Repo: "r", Tag: "t"})
			return err
		}},
	}
	for _, c := range cases {
		err := c.call()
		code := connect.CodeOf(err)
		// No builder in this harness -> Unavailable. A malformed archive may be
		// reported as InvalidArgument first; both are correct, Internal is not.
		if code != connect.CodeUnavailable && code != connect.CodeInvalidArgument && code != connect.CodeFailedPrecondition {
			t.Errorf("%s code = %v, want Unavailable/InvalidArgument/FailedPrecondition (err=%v)", c.name, code, err)
		}
	}

	// BuildImageStream is a stream: it must fail the same way (not hang).
	stream, err := h.Client.BuildImageStream(ctx, &easyopsv1.BuildImageRequest{Archive: archive, Repo: "r", Tag: "t"})
	if err == nil {
		_, err = stream.Receive()
		_ = stream.Close()
	}
	if code := connect.CodeOf(err); code != connect.CodeUnavailable && code != connect.CodeInvalidArgument && code != connect.CodeFailedPrecondition {
		t.Errorf("BuildImageStream code = %v (err=%v)", code, err)
	}
}

// TestRPC_WatchWorkspaceStream covers the live stream: a self-contained snapshot
// frame that really carries the owner's objects, then a frame flagged for the
// kind that changed.
func TestRPC_WatchWorkspaceStream(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	owner := "alice"

	// Create the object FIRST. A proto3 repeated field cannot distinguish
	// "empty" from "absent", so asserting on an empty namespace proves nothing:
	// the frame would look the same whether the list was fetched (and empty) or
	// skipped. With a real object present, a missing list is unambiguous.
	name := fmt.Sprintf("rpc-watch-%d", time.Now().UnixNano())
	if _, err := h.Client.CreateSandbox(ctx, &easyopsv1.CreateSandboxRequest{
		Owner: owner, Name: name, Image: fixture(t, "worker:1"),
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() {
		_, _ = h.Client.DeleteSandbox(context.Background(), &easyopsv1.DeleteSandboxRequest{Owner: owner, Name: name})
	}()

	stream, err := h.Client.WatchWorkspace(ctx, &easyopsv1.WatchWorkspaceRequest{Owner: owner})
	if err != nil {
		t.Fatalf("WatchWorkspace: %v", err)
	}
	defer stream.Close()

	first, err := stream.Receive()
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if !first.GetSnapshot() {
		t.Errorf("the first frame must be the snapshot frame: %+v", first)
	}
	// Every frame is self-contained: the snapshot must carry the owner's lists.
	var seen bool
	for _, s := range first.GetSandboxes() {
		if s.GetName() == name {
			seen = true
		}
	}
	if !seen {
		t.Errorf("the snapshot frame must carry the owner's sandboxes (looked for %q): %+v", name, first)
	}

	// Creating a second sandbox must produce a frame flagged for sandboxes.
	name2 := fmt.Sprintf("rpc-watch2-%d", time.Now().UnixNano())
	if _, err := h.Client.CreateSandbox(ctx, &easyopsv1.CreateSandboxRequest{
		Owner: owner, Name: name2, Image: fixture(t, "worker:1"),
	}); err != nil {
		t.Fatalf("CreateSandbox (2): %v", err)
	}
	defer func() {
		_, _ = h.Client.DeleteSandbox(context.Background(), &easyopsv1.DeleteSandboxRequest{Owner: owner, Name: name2})
	}()

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		f, err := stream.Receive()
		if err != nil {
			t.Fatalf("next frame: %v", err)
		}
		if f.GetSandboxesChanged() {
			// Every frame is self-contained: it must carry the full list.
			var found bool
			for _, sb := range f.GetSandboxes() {
				if sb.GetName() == name2 {
					found = true
				}
			}
			if !found {
				t.Errorf("a sandboxes_changed frame must be self-contained (missing %q)", name2)
			}
			return
		}
	}
	t.Fatal("no sandboxes_changed frame arrived after creating a sandbox")
}

// TestRPC_ApplyManifestRejectsGarbage: a bad manifest must be InvalidArgument,
// never Internal, and must not damage the service.
func TestRPC_ApplyManifestRejectsGarbage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, err := h.Client.ApplyServiceManifest(ctx, &easyopsv1.ApplyServiceManifestRequest{
		Owner: "alice", Name: "whatever", Yaml: "this is not yaml: [",
	})
	if err == nil {
		t.Fatal("a malformed manifest must be rejected")
	}
	if code := connect.CodeOf(err); code == connect.CodeInternal {
		t.Errorf("a malformed manifest must not be Internal (got %v: %v)", code, err)
	}
}
