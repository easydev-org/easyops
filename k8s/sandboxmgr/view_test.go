package sandboxmgr

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestToSandboxDiagnostics checks the pod diagnostics a Failed sandbox surfaces:
// exit code, terminated reason, lastState reason, container name and times.
func TestToSandboxDiagnostics(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "oom", Image: "img:1", Creator: "alice"}); err != nil {
		t.Fatal(err)
	}
	pod := seedSandboxPod(t, c, ctx, "oom")
	started := metav1.Now()
	finished := metav1.NewTime(started.Add(3_000_000_000))
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "worker", RestartCount: 2, Ready: false,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", ExitCode: 1, StartedAt: started, FinishedAt: finished,
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "OOMKilled", ExitCode: 137,
		}},
	}}
	if _, err := c.cs.CoreV1().Pods("worker").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Get(ctx, "oom")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Phase != "Failed" || got.ExitCode != 1 || got.TerminatedReason != "Error" {
		t.Fatalf("phase/exit/reason = %q/%d/%q", got.Phase, got.ExitCode, got.TerminatedReason)
	}
	if got.LastStateReason != "OOMKilled" {
		t.Fatalf("lastState = %q, want OOMKilled", got.LastStateReason)
	}
	if got.Container != "worker" || got.StartedAt == 0 || got.FinishedAt == 0 {
		t.Fatalf("container/times = %q/%d/%d", got.Container, got.StartedAt, got.FinishedAt)
	}
	if got.Restarts != 2 {
		t.Fatalf("restarts = %d, want 2", got.Restarts)
	}
}

// TestEventsBestEffort checks that Events never errors: with no events it
// returns nil, and it renders reason+message when events exist.
func TestEventsBestEffort(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "ev", Image: "img:1"}); err != nil {
		t.Fatal(err)
	}
	if ev := c.Events(ctx, "ev", 20); len(ev) != 0 {
		t.Fatalf("no events expected, got %v", ev)
	}
	pod := seedSandboxPod(t, c, ctx, "ev")
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev.1", Namespace: "worker"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name},
		Reason:         "BackOff", Message: "Back-off restarting failed container",
		LastTimestamp: metav1.Now(),
	}
	if _, err := c.cs.CoreV1().Events("worker").Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	got := c.Events(ctx, "ev", 20)
	if len(got) != 1 || got[0] != "BackOff: Back-off restarting failed container" {
		t.Fatalf("events = %v", got)
	}
}

// TestSandboxURLIsTheServiceNotThePod pins the stable-address rule.
//
// toSandbox used to build the URL from the POD's name (wm-x-<rs>-<pod>), so the
// reported address changed whenever the Deployment replaced its pod — a rollout
// or a crash-restart silently broke any caller that had cached the URL. The
// Service is named after the Deployment, which is stable for the sandbox's
// whole life.
func TestSandboxURLIsTheServiceNotThePod(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "urltest", Image: "img:1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulate the Deployment having created a pod (the fake client does not run
	// a ReplicaSet controller).
	res := resourceName("urltest")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: res + "-58fd8b7767-q25s4", Namespace: c.namespace,
			Labels:      map[string]string{LabelManaged: "1", LabelName: "urltest"},
			Annotations: map[string]string{AnnoImage: "img:1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if _, err := c.cs.CoreV1().Pods(c.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	sb, found, err := c.Get(ctx, "urltest")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	want := c.ServiceDNS(res)
	if sb.URL != want {
		t.Fatalf("URL = %q, want the Service address %q (not the pod name)", sb.URL, want)
	}
	if strings.Contains(sb.URL, "58fd8b7767") {
		t.Fatalf("URL must never contain the ReplicaSet hash: %q", sb.URL)
	}
}
