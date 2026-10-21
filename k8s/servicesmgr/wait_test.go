package servicesmgr

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newWaitTestClient() *Client {
	return NewWithClientset(fake.NewSimpleClientset(), Config{Namespace: "worker"})
}

// seedPod creates a pod matching a service's Deployment selector so
// deploymentReadiness can inspect it.
func seedPod(t *testing.T, c *Client, name string, mutate func(*corev1.Pod)) {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-pod", Namespace: "worker",
			Labels: map[string]string{"app": name},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "svc", Image: "img:1"}}},
	}
	if mutate != nil {
		mutate(pod)
	}
	if _, err := c.cs.CoreV1().Pods("worker").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}
}

func deployForWait(t *testing.T, c *Client, name string) {
	t.Helper()
	if _, err := c.Deploy(context.Background(), Spec{Name: name, Image: "img:1", Creator: "t", ContainerPort: 8080}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
}

// TestWaitReadyImagePullBackOff checks a deterministic image-pull failure is
// surfaced immediately (Failed=true) with the pod reason.
func TestWaitReadyImagePullBackOff(t *testing.T) {
	c := newWaitTestClient()
	deployForWait(t, c, "app")
	seedPod(t, c, "app", func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "svc", Ready: false,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ImagePullBackOff", Message: "Back-off pulling image",
			}},
		}}
	})
	res, err := c.WaitReady(context.Background(), "app", 5*time.Second)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if !res.Failed {
		t.Fatalf("Failed = false, want true (%+v)", res)
	}
	if !strings.Contains(res.Reason, "ImagePullBackOff") {
		t.Fatalf("reason = %q, want ImagePullBackOff", res.Reason)
	}
}

// TestWaitReadyUnschedulable checks an unschedulable pod is a deterministic
// failure reported as FailedScheduling.
func TestWaitReadyUnschedulable(t *testing.T) {
	c := newWaitTestClient()
	deployForWait(t, c, "app")
	seedPod(t, c, "app", func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		p.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: "Unschedulable", Message: "0/3 nodes are available",
		}}
	})
	res, err := c.WaitReady(context.Background(), "app", 5*time.Second)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Reason, "FailedScheduling") {
		t.Fatalf("res = %+v, want Failed=true reason=FailedScheduling", res)
	}
}

// TestWaitReadyReady checks a Deployment with a ready replica returns Ready.
func TestWaitReadyReady(t *testing.T) {
	c := newWaitTestClient()
	ctx := context.Background()
	deployForWait(t, c, "app")
	dep, err := c.cs.AppsV1().Deployments("worker").Get(ctx, "app", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dep.Status.ReadyReplicas = 1
	if _, err := c.cs.AppsV1().Deployments("worker").UpdateStatus(ctx, dep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := c.WaitReady(ctx, "app", 5*time.Second)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if !res.Ready || res.Failed {
		t.Fatalf("res = %+v, want Ready=true Failed=false", res)
	}
}

// TestWaitReadyTimeoutNonFatal checks that "still not ready" (no pod yet) is NOT
// a hard failure: Failed=false after the bounded wait.
func TestWaitReadyTimeoutNonFatal(t *testing.T) {
	c := newWaitTestClient()
	ctx := context.Background()
	deployForWait(t, c, "app")
	// Delete any pods so readiness sees "no pod yet" (keeps waiting).
	_ = c.cs.CoreV1().Pods("worker").DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})
	res, err := c.WaitReady(ctx, "app", 1*time.Second)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if res.Failed {
		t.Fatalf("res = %+v, want Failed=false on a plain timeout", res)
	}
}

var _ = appsv1.Deployment{}
