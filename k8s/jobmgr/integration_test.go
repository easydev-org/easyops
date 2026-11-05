//go:build integration

// Package jobmgr's integration suite runs against a REAL Kubernetes cluster.
//
// It is guarded by the `integration` build tag so `go test ./...` stays fast and
// cluster-free; CI runs it in a separate job against a k3d cluster:
//
//	k3d cluster create ...
//	KUBECONFIG=... EASYOPS_KUBECONFIG=... go test -tags=integration ./k8s/jobmgr/
//
// Everything here asserts BEHAVIOUR a fake clientset cannot: that the kubelet
// actually STARTS the container (security context, UID, read-only rootfs, writable
// mounts), that logs come back, and that the exit code is reported.
package jobmgr

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// clusterConfig fails the test when no cluster is configured.
func clusterConfig(t *testing.T) Config {
	t.Helper()
	if os.Getenv("EASYOPS_KUBECONFIG") == "" && os.Getenv("KUBECONFIG") == "" {
		t.Skip("no cluster configured (set EASYOPS_KUBECONFIG or KUBECONFIG)")
	}
	ns := os.Getenv("EASYOPS_TEST_NAMESPACE")
	if ns == "" {
		ns = "easyops-ci"
	}
	return Config{Namespace: ns}
}

// waitForTerminal polls until the job reaches a terminal state.
func waitForTerminal(t *testing.T, c *Client, name string) *Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		j, err := c.Get(context.Background(), name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if j.Done {
			return j
		}
		time.Sleep(2 * time.Second)
	}
	// A job that never leaves `active` is a failure mode worth reporting with
	// the pod's diagnostics, not just "timeout".
	j, _ := c.Get(context.Background(), name)
	t.Fatalf("job %s did not finish in time: %+v", name, j)
	return nil
}

// TestUntrustedJobActuallyRuns is the point of the integration suite: a
// hardened job (non-root, read-only rootfs, no caps, no SA token) must still
// START and WRITE to /workspace.
//
// Without an explicit runAsUser this fails with CreateContainerConfigError on
// every root image — a failure a fake clientset cannot catch.
func TestUntrustedJobActuallyRuns(t *testing.T) {
	cfg := clusterConfig(t)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	name := fmt.Sprintf("easyops-int-write-%d", time.Now().UnixNano())

	j, err := c.Create(context.Background(), Spec{
		Name: name, Image: "busybox:1.36",
		Command: []string{"/bin/sh"}, Args: []string{"-c", `
			set -e
			echo "uid=$(id -u) gid=$(id -g)"
			echo hello > /workspace/proof.txt
			cat /workspace/proof.txt
			echo tmp > /tmp/proof.txt
			# The root fs must be READ-ONLY: this write has to fail.
			if touch /proof.txt 2>/dev/null; then
			  echo "ROOTFS_WRITABLE"; exit 1
			fi
			echo done
		`},
		Untrusted:               true,
		TTLSecondsAfterFinished: 60,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = c.Delete(context.Background(), j.Name, "background") }()

	done := waitForTerminal(t, c, j.Name)
	if done.State != "succeeded" {
		logs, _ := c.Logs(context.Background(), j.Name, "", 200)
		t.Fatalf("job state = %s (reason=%q message=%q)\n--- logs ---\n%s", done.State, done.Reason, done.Message, logs)
	}
	logs, err := c.Logs(context.Background(), j.Name, "", 200)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	// The container really ran as the configured non-root UID.
	if !strings.Contains(logs, fmt.Sprintf("uid=%d", DefaultUntrustedID)) {
		t.Errorf("expected the job to run as uid %d:\n%s", DefaultUntrustedID, logs)
	}
	if !strings.Contains(logs, "hello") || !strings.Contains(logs, "done") {
		t.Errorf("the step did not produce its output:\n%s", logs)
	}
	if strings.Contains(logs, "ROOTFS_WRITABLE") {
		t.Errorf("the root filesystem must be read-only:\n%s", logs)
	}
}

// TestUntrustedJobCannotReachTheAPI proves the security property in a real
// cluster: no ServiceAccount token is mounted, so the in-cluster API and the
// token file are unreachable.
func TestUntrustedJobCannotReachTheAPI(t *testing.T) {
	cfg := clusterConfig(t)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	name := fmt.Sprintf("easyops-int-nosa-%d", time.Now().UnixNano())

	j, err := c.Create(context.Background(), Spec{
		Name: name, Image: "busybox:1.36",
		Command: []string{"/bin/sh"}, Args: []string{"-c", `
			set -e
			if [ -e /var/run/secrets/kubernetes.io/serviceaccount/token ]; then
			  echo "TOKEN_PRESENT"; exit 1
			fi
			echo "no-token"
		`},
		Untrusted:               true,
		TTLSecondsAfterFinished: 60,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = c.Delete(context.Background(), j.Name, "background") }()

	done := waitForTerminal(t, c, j.Name)
	logs, _ := c.Logs(context.Background(), j.Name, "", 200)
	if done.State != "succeeded" {
		t.Fatalf("job = %s (reason=%q)\n%s", done.State, done.Reason, logs)
	}
	if strings.Contains(logs, "TOKEN_PRESENT") {
		t.Fatalf("an untrusted job must not have a ServiceAccount token:\n%s", logs)
	}
}

// TestFailingJobReportsExitCode pins the failure path: a non-zero exit must
// surface as a failed job with the pod's exit code, which is what the CI
// orchestrator turns into a red check.
func TestFailingJobReportsExitCode(t *testing.T) {
	cfg := clusterConfig(t)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	name := fmt.Sprintf("easyops-int-fail-%d", time.Now().UnixNano())

	j, err := c.Create(context.Background(), Spec{
		Name: name, Image: "busybox:1.36",
		Command: []string{"/bin/sh"}, Args: []string{"-c", "echo boom; exit 7"},
		Untrusted:               true,
		TTLSecondsAfterFinished: 60,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = c.Delete(context.Background(), j.Name, "background") }()

	done := waitForTerminal(t, c, j.Name)
	if done.State != "failed" {
		t.Fatalf("state = %s, want failed", done.State)
	}
	if done.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", done.ExitCode)
	}
}

// TestWorkspacePVCIsMountable checks the cache path: a PVC is provisioned and
// mounted at /workspace, and a file written by one job is visible to the next.
//
// k3d ships local-path provisioning, so this needs no storage class setup.
func TestWorkspacePVCIsMountable(t *testing.T) {
	cfg := clusterConfig(t)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	claim := fmt.Sprintf("easyops-int-cache-%d", time.Now().UnixNano())
	if err := c.EnsureWorkspacePVC(context.Background(), claim, "100Mi"); err != nil {
		t.Fatalf("ensure pvc: %v", err)
	}
	defer func() { _ = c.DeleteWorkspacePVC(context.Background(), claim) }()

	write := fmt.Sprintf("easyops-int-w-%d", time.Now().UnixNano())
	j, err := c.Create(context.Background(), Spec{
		Name: write, Image: "busybox:1.36",
		Command: []string{"/bin/sh"}, Args: []string{"-c", `echo persisted > /workspace/cache.txt`},
		Untrusted: true, Workspace: claim, TTLSecondsAfterFinished: 60,
	})
	if err != nil {
		t.Fatalf("create writer: %v", err)
	}
	if done := waitForTerminal(t, c, j.Name); done.State != "succeeded" {
		logs, _ := c.Logs(context.Background(), j.Name, "", 200)
		t.Fatalf("writer = %s (reason=%q)\n%s", done.State, done.Reason, logs)
	}

	read := fmt.Sprintf("easyops-int-r-%d", time.Now().UnixNano())
	j2, err := c.Create(context.Background(), Spec{
		Name: read, Image: "busybox:1.36",
		Command: []string{"/bin/sh"}, Args: []string{"-c", "cat /workspace/cache.txt"},
		Untrusted: true, Workspace: claim, TTLSecondsAfterFinished: 60,
	})
	if err != nil {
		t.Fatalf("create reader: %v", err)
	}
	if done := waitForTerminal(t, c, j2.Name); done.State != "succeeded" {
		logs, _ := c.Logs(context.Background(), j2.Name, "", 200)
		t.Fatalf("reader = %s (reason=%q)\n%s", done.State, done.Reason, logs)
	}
	logs, _ := c.Logs(context.Background(), j2.Name, "", 200)
	if !strings.Contains(logs, "persisted") {
		t.Fatalf("the PVC did not survive between jobs:\n%s", logs)
	}
}

// TestCompanionServiceIsResolvable pins the `services:` contract in a real
// cluster: a companion is REACHABLE from the job by its name.
//
// Two lessons from writing this:
//
//   - The first version created the Job and its companion at the same instant,
//     so the job started before the Service had endpoints. The manager now
//     creates the companion first and waits for a ready endpoint.
//   - The assertion must be a CONNECTION, not `nslookup`: busybox's nslookup
//     with a single-label name is unreliable (musl + search domains), and DNS
//     answering says nothing about the port being reachable. Connecting with a
//     short retry is what a real job would do, and it is deterministic.
func TestCompanionServiceIsResolvable(t *testing.T) {
	cfg := clusterConfig(t)
	// Give the companion time to pull on a cold node.
	cfg.ServiceReadyTimeout = 90 * time.Second
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	name := fmt.Sprintf("easyops-int-svc-%d", time.Now().UnixNano())

	j, err := c.Create(context.Background(), Spec{
		Name: name, Image: "busybox:1.36",
		Command: []string{"/bin/sh"}, Args: []string{"-c", `
			set -e
			# Connect to the companion by NAME. A few retries because kube-proxy's
			# rules can lag the endpoint by a moment; a job would do the same.
			for i in $(seq 1 30); do
			  if wget -q -T 2 -O /tmp/page http://pg/ 2>/dev/null; then
			    grep -qi "welcome to nginx" /tmp/page && { echo "service-ok"; exit 0; }
			  fi
			  sleep 1
			done
			echo "NO_SERVICE"
			exit 1
		`},
		Untrusted: true,
		Services: []ServiceSpec{{
			Name: "pg", Image: "nginx:1.27-alpine", Ports: []int32{80},
		}},
		TTLSecondsAfterFinished: 60,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = c.Delete(context.Background(), j.Name, "background") }()

	done := waitForTerminal(t, c, j.Name)
	logs, _ := c.Logs(context.Background(), j.Name, "", 200)
	if done.State != "succeeded" {
		t.Fatalf("job = %s (reason=%q)\n%s", done.State, done.Reason, logs)
	}
	if !strings.Contains(logs, "service-ok") {
		t.Fatalf("the companion was not reachable by name:\n%s", logs)
	}
}
