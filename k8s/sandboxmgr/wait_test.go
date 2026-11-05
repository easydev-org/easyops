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
