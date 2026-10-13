package opssvc

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/internal/runtimeprofiles"
	"github.com/easy-platform/easyops/internal/sandboxmgr"
	"github.com/easy-platform/easyops/internal/servicesmgr"
)

// TestResolveSandboxVolumesAuthorizesOwnership: a sandbox may only mount PVCs
// owned by the caller's tenant; another tenant's or an unknown PVC -> NotFound.
func TestResolveSandboxVolumesAuthorizesOwnership(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: "mine", Namespace: "worker",
			Labels:      map[string]string{"workspace/pvc": "1"},
			Annotations: map[string]string{"workspace/pvc-creator": "acme"},
		}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: "other", Namespace: "worker",
			Labels:      map[string]string{"workspace/pvc": "1"},
			Annotations: map[string]string{"workspace/pvc-creator": "globex"},
		}},
	)
	s := New(Deps{
		Sandbox:  sandboxmgr.NewWithClientset(cs, sandboxmgr.Config{Namespace: "worker"}),
		Services: servicesmgr.NewWithClientset(cs, servicesmgr.Config{Namespace: "worker"}),
		Runtime:  runtimeprofiles.DefaultSettings(),
	})
	mount := func(pvc, path string) []*opsv1.VolumeMountSpec {
		return []*opsv1.VolumeMountSpec{{Pvc: pvc, MountPath: path}}
	}

	if _, err := s.resolveSandboxVolumes(context.Background(), "acme", mount("other", "/d")); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("other-tenant PVC: code = %v, want NotFound", connect.CodeOf(err))
	}
	if _, err := s.resolveSandboxVolumes(context.Background(), "acme", mount("nope", "/d")); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown PVC: code = %v, want NotFound", connect.CodeOf(err))
	}
	vols, err := s.resolveSandboxVolumes(context.Background(), "acme", mount("mine", "/d"))
	if err != nil || len(vols) != 1 || vols[0].PVC != "mine" {
		t.Fatalf("own PVC: vols=%v err=%v", vols, err)
	}
	if _, err := s.resolveSandboxVolumes(context.Background(), "acme", mount("mine", "rel")); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("relative path: code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// TestResolveSandboxVolumesNoServicesBackend: with volumes requested but the
// services backend NIL, the resolver returns Unavailable (never a nil deref).
func TestResolveSandboxVolumesNoServicesBackend(t *testing.T) {
	s := New(Deps{
		Sandbox: sandboxmgr.NewWithClientset(fake.NewSimpleClientset(), sandboxmgr.Config{Namespace: "worker"}),
		Runtime: runtimeprofiles.DefaultSettings(),
	})
	_, err := s.resolveSandboxVolumes(context.Background(), "acme",
		[]*opsv1.VolumeMountSpec{{Pvc: "mine", MountPath: "/d"}})
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("no services backend: code = %v, want Unavailable", connect.CodeOf(err))
	}
}
