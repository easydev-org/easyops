package servicesmgr

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func pendingPVC(name string) *corev1.PersistentVolumeClaim {
	sc := "workspace-local"
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "dev",
			Labels:            map[string]string{LabelPVC: "1", LabelPVCName: name},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-90 * time.Second)),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("10Gi"),
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
}

// TestPendingPVCExplainsItself pins the reason this field exists: a PVC has only
// Pending/Bound/Lost, and Pending covers BOTH "still provisioning" and
// "provisioning failed". Without `message` a caller cannot tell them apart.
func TestPendingPVCExplainsItself(t *testing.T) {
	cs := fake.NewSimpleClientset(pendingPVC("waiting"))
	c := NewWithClientset(cs, Config{Namespace: "dev"})
	ctx := context.Background()

	// A Normal waiting event: this is the healthy "not yet" case.
	_, _ = cs.CoreV1().Events("dev").Create(ctx, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "waiting.1", Namespace: "dev"},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: "waiting"},
		Type:           corev1.EventTypeNormal,
		Reason:         "WaitForFirstConsumer",
		Message:        "waiting for first consumer to be created before binding",
		LastTimestamp:  metav1.Now(),
	}, metav1.CreateOptions{})

	v, err := c.GetPVC(ctx, "waiting")
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "Pending" {
		t.Fatalf("phase = %q", v.Phase)
	}
	if v.Message == "" {
		t.Error("a Pending claim must explain itself")
	}
	if !containsStr(v.Message, "waiting for first consumer") {
		t.Errorf("message = %q", v.Message)
	}
	if v.AgeSeconds < 60 {
		t.Errorf("ageSeconds = %d, want ~90", v.AgeSeconds)
	}
}

// TestWarningBeatsNormalEvent: a claim that emitted both a Normal "Provisioning"
// and a Warning "ProvisioningFailed" must report the WARNING — it is the one
// that explains why it will never bind.
func TestWarningBeatsNormalEvent(t *testing.T) {
	cs := fake.NewSimpleClientset(pendingPVC("broken"))
	c := NewWithClientset(cs, Config{Namespace: "dev"})
	ctx := context.Background()

	base := metav1.NewTime(time.Now().Add(-time.Minute))
	_, _ = cs.CoreV1().Events("dev").Create(ctx, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "broken.1", Namespace: "dev"},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: "broken"},
		Type:           corev1.EventTypeNormal, Reason: "Provisioning",
		Message: "External provisioner is provisioning volume", LastTimestamp: base,
	}, metav1.CreateOptions{})
	_, _ = cs.CoreV1().Events("dev").Create(ctx, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "broken.2", Namespace: "dev"},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: "broken"},
		Type:           corev1.EventTypeWarning, Reason: "ProvisioningFailed",
		Message:       `failed to provision volume with StorageClass "local-path"`,
		LastTimestamp: metav1.NewTime(time.Now()),
	}, metav1.CreateOptions{})

	v, err := c.GetPVC(ctx, "broken")
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(v.Message, "ProvisioningFailed") {
		t.Errorf("must surface the WARNING event, got %q", v.Message)
	}
	if !containsStr(v.Message, "local-path") {
		t.Errorf("warning message lost: %q", v.Message)
	}
}

// TestBoundPVCSkipsEvents: the healthy case must not pay for an Events query.
func TestBoundPVCSkipsEvents(t *testing.T) {
	p := pendingPVC("ok")
	p.Status.Phase = corev1.ClaimBound
	p.Spec.VolumeName = "pv-123"
	p.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}
	cs := fake.NewSimpleClientset(p)
	c := NewWithClientset(cs, Config{Namespace: "dev"})

	v, err := c.GetPVC(context.Background(), "ok")
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "Bound" || v.BoundVolume != "pv-123" || v.Capacity == "" {
		t.Errorf("bound view = %+v", v)
	}
	if v.Message != "" {
		t.Errorf("a Bound claim needs no explanation, got %q", v.Message)
	}
}

// foreignPVC is a claim that easyops did NOT create: no workspace/pvc label and
// no creator annotation (the platform's own postgres-data, garage-data, ...).
func foreignPVC(name string) *corev1.PersistentVolumeClaim {
	p := pendingPVC(name)
	p.Labels = nil
	p.Annotations = nil
	return p
}

// TestListPVCDefaultsToManagedOnly pins the tenant-safe default: a tenant must
// not be shown the platform's own claims.
func TestListPVCDefaultsToManagedOnly(t *testing.T) {
	cs := fake.NewSimpleClientset(pendingPVC("mine"), foreignPVC("postgres-data"))
	c := NewWithClientset(cs, Config{Namespace: "dev"})

	got, err := c.ListPVCs(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "mine" {
		names := make([]string, len(got))
		for i, p := range got {
			names[i] = p.Name
		}
		t.Fatalf("default list = %v, want only [mine]", names)
	}
}

// TestListPVCIncludeAll: the explicit opt-in must return everything, including
// claims created outside easyops (which have no creator).
func TestListPVCIncludeAll(t *testing.T) {
	cs := fake.NewSimpleClientset(pendingPVC("mine"), foreignPVC("postgres-data"))
	c := NewWithClientset(cs, Config{Namespace: "dev"})

	got, err := c.ListPVCs(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("includeAll list = %d claims, want 2", len(got))
	}
	byName := map[string]PVC{}
	for _, p := range got {
		byName[p.Name] = p
	}
	if f, ok := byName["postgres-data"]; !ok {
		t.Error("includeAll must return the foreign claim")
	} else if f.Creator != "" {
		t.Errorf("a foreign claim has no creator, got %q", f.Creator)
	}
}

func containsStr(h, sub string) bool {
	return len(sub) == 0 || (len(h) >= len(sub) && indexOfStr(h, sub) >= 0)
}

func indexOfStr(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
