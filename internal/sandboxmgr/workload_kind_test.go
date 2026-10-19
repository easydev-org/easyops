package sandboxmgr

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSandboxIsADeploymentNotABarePod pins the workload-kind rule: a sandbox is
// created as a Deployment (which owns its Pod) — NEVER as a bare Pod. A bare Pod
// has no controller to keep it running and is not allowed in easyops.
func TestSandboxIsADeploymentNotABarePod(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "kind", Image: "img:1", Creator: "alice"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	res := resourceName("kind")

	// The Deployment exists...
	if _, err := c.cs.AppsV1().Deployments("worker").Get(ctx, res, metav1.GetOptions{}); err != nil {
		t.Fatalf("sandbox Deployment missing: %v", err)
	}
	// ...and NO bare pod with the sandbox name exists (a bare pod would be an
	// object with the sandbox's name that has no owner).
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, res, metav1.GetOptions{})
	if err == nil {
		t.Fatalf("a bare Pod named %q must NOT exist (sandboxes are Deployments): %+v", res, pod)
	}
}

// TestSandboxDeploymentSingleReplica: the sandbox Deployment runs exactly one
// replica (a session-scoped worker), and its pod template restarts Always.
func TestSandboxDeploymentSingleReplica(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "one", Image: "img:1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	d, err := c.cs.AppsV1().Deployments("worker").Get(ctx, resourceName("one"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want 1", d.Spec.Replicas)
	}
	if got := d.Spec.Template.Spec.RestartPolicy; string(got) != "Always" {
		t.Fatalf("restartPolicy = %q, want Always (a Deployment requires it)", got)
	}
}
