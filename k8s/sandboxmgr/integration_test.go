package sandboxmgr

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSandboxLifecycle is the horizontal test for sandboxmgr: a Deployment +
// Service + Secret triple that actually comes up, serves its port, and is fully
// removed.
//
// A fake clientset cannot catch any of this: whether the pod starts at all,
// whether the Service port maps to the worker port, whether the Secret is
// readable, and whether Delete really removes all three objects.
func TestSandboxLifecycle(t *testing.T) {
	c, _ := newClusterClient(t)
	ctx := context.Background()
	name := fmt.Sprintf("sbx-%d", time.Now().UnixNano())
	img := fixtureImage(t, "worker:1")

	sb, token, err := c.Create(ctx, Spec{
		Name: name, Image: img, Creator: "alice", Session: "s1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _, _ = c.Delete(ctx, name) }()

	if token == "" {
		t.Fatal("Create must return a bearer token")
	}
	if sb.URL == "" || !strings.HasPrefix(sb.URL, "http://") {
		t.Fatalf("url = %q", sb.URL)
	}

	// 1. It becomes ready — the assertion a fake cannot make.
	ready := waitSandboxReady(t, c, name, 3*time.Minute)
	if ready.Name != name {
		t.Errorf("name = %q, want %q", ready.Name, name)
	}
	if ready.Phase != "Running" {
		t.Errorf("phase = %q, want Running", ready.Phase)
	}

	// 2. Resolve reads the Service URL and the SAME token Create minted, but from
	//    the Secret (i.e. through k8s, not from process memory).
	//
	//    The URL must ALSO be the sandbox's stable Service address, not the
	//    pod's: using the pod name made the reported URL change on every rollout
	//    or crash-restart, breaking any caller that cached it.
	url, tok, err := c.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tok != token {
		t.Errorf("Resolve token does not match the token Create returned")
	}
	if url != ready.URL {
		t.Errorf("resolve url = %q, ready url = %q", url, ready.URL)
	}
	// The URL must be the Deployment's Service, never the pod's: a pod name
	// changes on every rollout, so a cached URL would break.
	wantHost := resourceName(name) + "." + c.Namespace() + ".svc.cluster.local"
	if !strings.Contains(url, wantHost) {
		t.Errorf("url = %q, want the stable Service host %q (not the pod name)", url, wantHost)
	}
	for _, pod := range strings.Split(url, ".") {
		if strings.Count(pod, "-") >= 4 {
			t.Errorf("url %q looks like it contains a pod name", url)
			break
		}
	}

	// 3. The worker is reachable THROUGH THE SERVICE on port 80 and echoes the
	//    same token: that proves the targetPort mapping and Secret injection.
	body := runInCluster(t, c, img, "wget -q -T 5 -O - http://"+strings.TrimPrefix(url, "http://")+"/")
	if !strings.Contains(body, "sandbox-ok") {
		t.Errorf("worker response = %q", body)
	}
	if !strings.Contains(body, "token="+token) {
		t.Errorf("the worker did not receive WORKER_TOKEN from the Secret: %q", body)
	}

	// 4. Logs are readable from the real pod. The fixture logs one line at
	//    startup (busybox httpd does not log requests), which is enough to prove
	//    the log path works end to end.
	lines, err := c.Logs(ctx, name, LogOptions{TailLines: 50})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if len(lines) == 0 {
		t.Errorf("expected the worker's startup log line, got none")
	}

	// 5. List sees it.
	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var seen bool
	for _, s := range list {
		if s.Name == name {
			seen = true
		}
	}
	if !seen {
		t.Error("created sandbox missing from List")
	}

	// 6. Delete removes Deployment + Service + Secret; a second Delete is a no-op.
	ok, err := c.Delete(ctx, name)
	if err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if _, found, err := c.Get(ctx, name); err != nil {
		t.Fatalf("get after delete: %v", err)
	} else if found {
		t.Error("the sandbox still exists after Delete")
	}
	if _, _, err := c.Resolve(ctx, name); err == nil {
		t.Error("the Secret must be gone after Delete")
	}
	if ok, err := c.Delete(ctx, name); err != nil || ok {
		t.Errorf("a second Delete must be a no-op: ok=%v err=%v", ok, err)
	}
}

// TestSandboxNameIsNeverReused pins ErrExists: creating over a live sandbox
// would destroy its workload and token.
func TestSandboxNameIsNeverReused(t *testing.T) {
	c, _ := newClusterClient(t)
	ctx := context.Background()
	name := fmt.Sprintf("sbx-dup-%d", time.Now().UnixNano())
	img := fixtureImage(t, "worker:1")

	if _, _, err := c.Create(ctx, Spec{Name: name, Image: img}); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _, _ = c.Delete(ctx, name) }()

	_, token2, err := c.Create(ctx, Spec{Name: name, Image: img})
	if err == nil {
		t.Fatal("creating over a live sandbox must fail")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want an already-exists error", err)
	}
	if token2 != "" {
		t.Error("a failed Create must not mint a token")
	}
}

// TestSandboxFailureIsDiagnosable: a broken image must surface a reason quickly
// instead of hanging. This is what the RPC layer shows an operator.
func TestSandboxFailureIsDiagnosable(t *testing.T) {
	c, _ := newClusterClient(t)
	ctx := context.Background()
	name := fmt.Sprintf("sbx-bad-%d", time.Now().UnixNano())
	// The registry host is reachable (the cluster is configured), but this repo
	// does not exist -> ImagePullBackOff.
	img := fixtureImage(t, "does-not-exist:1")

	if _, _, err := c.Create(ctx, Spec{Name: name, Image: img}); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _, _ = c.Delete(ctx, name) }()

	start := time.Now()
	err := c.WaitReady(ctx, name, 60*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("WaitReady must fail for an unpullable image")
	}
	if elapsed > 55*time.Second {
		t.Errorf("WaitReady waited %s; it should surface the failure early", elapsed)
	}
	sb, found, _ := c.Get(ctx, name)
	if !found {
		t.Fatal("the sandbox object should still exist")
	}
	if sb.Message == "" {
		t.Errorf("a broken sandbox must report a message (got %+v)", sb)
	}
}
