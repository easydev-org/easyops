package jobmgr

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestUntrustedJobHasNoServiceAccountToken is the CI security property: a job
// that runs a tenant's arbitrary script must not be able to talk to the
// Kubernetes API as the namespace's ServiceAccount.
func TestUntrustedJobHasNoServiceAccountToken(t *testing.T) {
	c, cs := newClient()
	j, err := c.Create(context.Background(), Spec{
		Name: "ci-step", Image: "golang:1.27",
		Command: []string{"/bin/sh"}, Args: []string{"-c", "go test ./..."},
		Untrusted: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := cs.BatchV1().Jobs("dev").Get(context.Background(), j.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec := got.Spec.Template.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("an untrusted job must not automount a ServiceAccount token")
	}
	if spec.ServiceAccountName != "" {
		t.Fatalf("service account = %q, want empty", spec.ServiceAccountName)
	}
	sc := spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("an untrusted job needs a security context")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("runAsNonRoot must be set")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must be set")
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Error("privileged must be explicitly false")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities must drop ALL, got %+v", sc.Capabilities)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccomp RuntimeDefault must be set")
	}
	// /workspace and /tmp must still be writable, or a read-only rootfs breaks
	// every real toolchain.
	mounts := map[string]bool{}
	for _, m := range spec.Containers[0].VolumeMounts {
		mounts[m.MountPath] = true
	}
	for _, p := range []string{"/workspace", "/tmp"} {
		if !mounts[p] {
			t.Errorf("missing writable mount %s (mounts=%v)", p, mounts)
		}
	}
	vols := map[string]corev1.VolumeSource{}
	for _, v := range spec.Volumes {
		vols[v.Name] = v.VolumeSource
	}
	if _, ok := vols["workspace"]; !ok {
		t.Errorf("a workspace volume is required: %v", vols)
	}
	if vols["workspace"].EmptyDir == nil {
		t.Errorf("an ephemeral job should use an emptyDir workspace: %v", vols["workspace"])
	}
}

// TestUntrustedJobRefusesServiceAccount: an explicit SA would hand the token
// back, so it is rejected rather than silently ignored.
func TestUntrustedJobRefusesServiceAccount(t *testing.T) {
	c, _ := newClient()
	_, err := c.Create(context.Background(), Spec{
		Name: "ci-step", Image: "x", ServiceAccountName: "builder", Untrusted: true,
	})
	if err == nil {
		t.Fatal("an untrusted job with a service_account must be refused")
	}
}

// TestTrustedJobIsUnchanged: the hardening is opt-in, so existing callers keep
// their behaviour.
func TestTrustedJobIsUnchanged(t *testing.T) {
	c, cs := newClient()
	j, err := c.Create(context.Background(), Spec{Name: "migration", Image: "x"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := cs.BatchV1().Jobs("dev").Get(context.Background(), j.Name, metav1.GetOptions{})
	if got.Spec.Template.Spec.AutomountServiceAccountToken != nil {
		t.Error("a trusted job must not have its automount forced")
	}
	if got.Spec.Template.Spec.Containers[0].SecurityContext != nil {
		t.Error("a trusted job must not gain a security context")
	}
}

// TestUntrustedJobWorkspacePVC: a cache PVC is mounted at /workspace when the
// caller asks for one.
func TestUntrustedJobWorkspacePVC(t *testing.T) {
	c, cs := newClient()
	j, err := c.Create(context.Background(), Spec{
		Name: "ci-step", Image: "x", Untrusted: true, Workspace: "ci-cache",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := cs.BatchV1().Jobs("dev").Get(context.Background(), j.Name, metav1.GetOptions{})
	var found bool
	for _, v := range got.Spec.Template.Spec.Volumes {
		if v.Name == "workspace" && v.PersistentVolumeClaim != nil {
			found = v.PersistentVolumeClaim.ClaimName == "ci-cache"
		}
	}
	if !found {
		t.Fatalf("workspace PVC not mounted: %+v", got.Spec.Template.Spec.Volumes)
	}
}

// TestCreateServicesCreatesDeploymentAndService pins the companion containers a
// CI job can reach by hostname.
func TestCreateServicesCreatesDeploymentAndService(t *testing.T) {
	c, cs := newClient()
	j, err := c.Create(context.Background(), Spec{
		Name: "ci-step", Image: "x", Untrusted: true,
		Services: []ServiceSpec{{
			Name: "pg", Image: "postgres:17",
			Env: map[string]string{"POSTGRES_PASSWORD": "t"}, Ports: []int32{5432},
		}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	deps, _ := cs.AppsV1().Deployments("dev").List(context.Background(), metav1.ListOptions{})
	if len(deps.Items) != 1 || deps.Items[0].Name != j.Name+"-pg" {
		t.Fatalf("deployments = %+v", deps.Items)
	}
	if deps.Items[0].Spec.Replicas == nil || *deps.Items[0].Spec.Replicas != 1 {
		t.Error("a companion must run one replica")
	}
	if deps.Items[0].Spec.Template.Spec.AutomountServiceAccountToken == nil ||
		*deps.Items[0].Spec.Template.Spec.AutomountServiceAccountToken {
		t.Error("a companion must not automount a token either")
	}
	svcs, _ := cs.CoreV1().Services("dev").List(context.Background(), metav1.ListOptions{})
	if len(svcs.Items) != 1 || svcs.Items[0].Name != "pg" {
		t.Fatalf("services = %+v", svcs.Items)
	}
	if len(svcs.Items[0].Spec.Ports) != 1 || svcs.Items[0].Spec.Ports[0].Port != 5432 {
		t.Errorf("ports = %+v", svcs.Items[0].Spec.Ports)
	}
}

// TestDeleteRemovesCompanions: deleting a job must not leak its companions.
func TestDeleteRemovesCompanions(t *testing.T) {
	c, cs := newClient()
	j, err := c.Create(context.Background(), Spec{
		Name: "ci-step", Image: "x", Untrusted: true,
		Services: []ServiceSpec{{Name: "pg", Image: "postgres:17", Ports: []int32{5432}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), j.Name, "background"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deps, _ := cs.AppsV1().Deployments("dev").List(context.Background(), metav1.ListOptions{}); len(deps.Items) != 0 {
		t.Fatalf("companion deployments leaked: %+v", deps.Items)
	}
	if svcs, _ := cs.CoreV1().Services("dev").List(context.Background(), metav1.ListOptions{}); len(svcs.Items) != 0 {
		t.Fatalf("companion services leaked: %+v", svcs.Items)
	}
}
