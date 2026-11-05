package servicesmgr

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// fixtureImage builds a registry-local image reference (pushed by
// scripts/fixture-images.sh). Using the cluster registry rather than Docker Hub
// keeps the suite hermetic and works with the pinned pull policy.
func fixtureImage(t *testing.T, repo string) string {
	t.Helper()
	reg := os.Getenv("EASYOPS_TEST_REGISTRY")
	if reg == "" {
		t.Skip("EASYOPS_TEST_REGISTRY is not set: run scripts/k3d-up.sh + scripts/fixture-images.sh")
	}
	return reg + "/easyops-fixtures/" + repo
}

// newClusterClient builds a client against the real cluster.
func newClusterClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(clusterConfig(t))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// waitReady polls until the service reports at least one ready replica.
func waitReady(t *testing.T, c *Client, name string, timeout time.Duration) Service {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		svc, err := c.Get(context.Background(), name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if svc.ReadyReplicas > 0 {
			return svc
		}
		if strings.Contains(svc.Message, "ImagePullBackOff") || strings.Contains(svc.Message, "ErrImagePull") {
			t.Fatalf("service %s cannot pull its image: %s", name, svc.Message)
		}
		time.Sleep(time.Second)
	}
	svc, _ := c.Get(context.Background(), name)
	t.Fatalf("service %s not ready in %s (phase=%q msg=%q replicas=%d/%d)",
		name, timeout, svc.PodPhase, svc.Message, svc.ReadyReplicas, svc.Replicas)
	return Service{}
}

// runInCluster runs a shell script in a throwaway pod and returns its stdout.
//
// The suite runs on the HOST, where `<svc>.<ns>.svc.cluster.local` does not
// resolve. "Reachable from inside the cluster" is the contract a caller relies
// on, so the assertion must run in a pod — which is also what proves the
// Service's targetPort mapping end to end.
func runInCluster(t *testing.T, c *Client, image, script string) string {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("probe-%d", time.Now().UnixNano()%1e9)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.namespace},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptrFalse(),
			Containers: []corev1.Container{{
				Name: "probe", Image: image,
				Command: []string{"/bin/sh", "-c", script},
			}},
		},
	}
	if _, err := c.cs.CoreV1().Pods(c.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create probe pod: %v", err)
	}
	defer func() {
		_ = c.cs.CoreV1().Pods(c.namespace).Delete(context.Background(), name, metav1.DeleteOptions{})
	}()

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		p, err := c.cs.CoreV1().Pods(c.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get probe pod: %v", err)
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			req := c.cs.CoreV1().Pods(c.namespace).GetLogs(name, &corev1.PodLogOptions{})
			rc, err := req.Stream(ctx)
			if err != nil {
				t.Fatalf("probe logs: %v", err)
			}
			defer rc.Close()
			var b strings.Builder
			buf := make([]byte, 32*1024)
			for {
				n, rerr := rc.Read(buf)
				if n > 0 {
					b.Write(buf[:n])
				}
				if rerr != nil {
					break
				}
			}
			return b.String()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("probe pod %s did not finish (phase=%s)", name, probePhase(t, c, name))
	return ""
}

func probePhase(t *testing.T, c *Client, name string) string {
	p, err := c.cs.CoreV1().Pods(c.namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return err.Error()
	}
	return string(p.Status.Phase)
}

func ptrFalse() *bool { f := false; return &f }
