package sandboxmgr

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestWaitReadyFailsFastOnImagePullBackOff: a container that cannot start must
// be reported immediately.
//
// A sandbox runs RestartPolicy=Always, so a broken pod stays in phase Pending
// with a container WAITING reason — never phase Failed. The old check tested
// phase == Failed only, so WaitReady burned its entire timeout before returning
// a generic "not healthy" error, and the operator never saw "ImagePullBackOff".
func TestWaitReadyFailsFastOnImagePullBackOff(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "pullfail", Image: "nope:1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	res := resourceName("pullfail")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: res + "-abc123-x1", Namespace: c.namespace,
			Labels: map[string]string{LabelManaged: "1", LabelName: "pullfail"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "worker",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}},
		},
	}
	if _, err := c.cs.CoreV1().Pods(c.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err := c.WaitReady(ctx, "pullfail", 30*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("WaitReady must fail for an unpullable image")
	}
	if !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Errorf("error = %v, want it to name ImagePullBackOff", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("WaitReady took %s; it must fail fast, not wait out the timeout", elapsed)
	}
}

// TestWaitReadyIgnoresContainerCreating: a normal startup is NOT a failure.
func TestWaitReadyIgnoresContainerCreating(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "starting", Image: "img:1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	res := resourceName("starting")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: res + "-abc123-x1", Namespace: c.namespace,
			Labels: map[string]string{LabelManaged: "1", LabelName: "starting"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "worker",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
			}},
		},
	}
	if _, err := c.cs.CoreV1().Pods(c.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// A short timeout: the point is that it must TIME OUT (not ready), not that
	// it returned a failure reason.
	err := c.WaitReady(ctx, "starting", 2*time.Second)
	if err == nil {
		t.Fatal("a pod that never becomes ready must time out")
	}
	if strings.Contains(err.Error(), "cannot start") {
		t.Errorf("ContainerCreating must not be reported as a startup failure: %v", err)
	}
}

// TestWaitReadyDoesNotRequireClusterDNS pins that the readiness check works from
// OUTSIDE the managed cluster.
//
// easyops runs with an explicit kubeconfig and often in a different cluster or
// namespace from the sandboxes it manages, so `<svc>.<ns>.svc.cluster.local` does
// not resolve in its own process. WaitReady used to require BOTH the pod IP and
// that cluster-only name, so it timed out for a perfectly healthy sandbox (the
// k3d run showed a 1/1 Running pod and a "not healthy within 2m" error).
//
// Here the pod IP is a closed port (nothing listens), so the assertion is about
// the DNS requirement, not about connectivity: WaitReady must reach the "not
// ready" state and TIME OUT rather than fail with an address-resolution error.
func TestWaitReadyDoesNotRequireClusterDNS(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "nodns", Image: "img:1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	res := resourceName("nodns")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: res + "-abc-x1", Namespace: c.namespace,
			Labels: map[string]string{LabelManaged: "1", LabelName: "nodns"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "127.0.0.1",
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "worker", Ready: true,
			}},
		},
	}
	if _, err := c.cs.CoreV1().Pods(c.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// 127.0.0.1:WorkerPort has no listener, so it times out — but the error must
	// be the generic "not healthy" one, never a DNS/lookup failure, and the check
	// must not depend on cluster DNS to even TRY.
	err := c.WaitReady(ctx, "nodns", 1500*time.Millisecond)
	if err == nil {
		t.Fatal("a closed worker port must not be reported ready")
	}
	if strings.Contains(strings.ToLower(err.Error()), "no such host") ||
		strings.Contains(err.Error(), "svc.cluster.local") {
		t.Fatalf("WaitReady must not depend on cluster DNS: %v", err)
	}
}
