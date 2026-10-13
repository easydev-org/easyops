package helmmgr

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestFillStatusWorkload checks a Deployment-like object reports Ready/replicas
// and, when not ready, surfaces the pod reason.
func TestFillStatusWorkload(t *testing.T) {
	c := &Client{namespace: "worker"}
	st := &ObjectStatus{Kind: "Deployment", Name: "app"}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"spec":   map[string]any{"replicas": int64(3)},
		"status": map[string]any{"readyReplicas": int64(1)},
	}}
	c.fillStatus(context.Background(), st, u)
	if st.Ready || st.Status != "Progressing" {
		t.Fatalf("ready=%v status=%q, want not-ready/Progressing", st.Ready, st.Status)
	}
	if st.DesiredReplicas != 3 || st.ReadyReplicas != 1 {
		t.Fatalf("replicas = %d/%d, want 1/3", st.ReadyReplicas, st.DesiredReplicas)
	}
}

// TestFillStatusPodDiagnostics checks a Pod's phase/ready/restarts/message.
func TestFillStatusPodDiagnostics(t *testing.T) {
	c := &Client{namespace: "worker"}
	st := &ObjectStatus{Kind: "Pod", Name: "app-1"}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"status": map[string]any{
			"phase":      "Running",
			"conditions": []any{map[string]any{"type": "Ready", "status": "False"}},
			"containerStatuses": []any{map[string]any{
				"restartCount": int64(5),
				"state":        map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
			}},
		},
	}}
	c.fillStatus(context.Background(), st, u)
	if st.Status != "Running" || st.Phase != "Running" || st.Ready {
		t.Fatalf("pod status = %q/%q ready=%v", st.Status, st.Phase, st.Ready)
	}
	if st.Restarts != 5 || st.Message != "CrashLoopBackOff" {
		t.Fatalf("restarts=%d message=%q", st.Restarts, st.Message)
	}
}

// TestFillStatusNonWorkload checks a Service (no spec.replicas) is Ready.
func TestFillStatusNonWorkload(t *testing.T) {
	c := &Client{namespace: "worker"}
	st := &ObjectStatus{Kind: "Service", Name: "svc"}
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Service"}}
	c.fillStatus(context.Background(), st, u)
	if !st.Ready || st.Status != "Ready" {
		t.Fatalf("service status = %q ready=%v", st.Status, st.Ready)
	}
}

// TestObjectLogsRejectsNonPod checks logs are only served for Pod objects.
func TestObjectLogsRejectsNonPod(t *testing.T) {
	c := &Client{namespace: "worker"}
	if _, err := c.ObjectLogs(context.Background(), "Deployment", "app", LogOptions{}); err == nil {
		t.Fatal("expected an error for a non-Pod kind")
	}
}
