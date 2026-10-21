package opssvc

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/k8s/runtimeprofiles"
	"github.com/easy-platform/easyops/k8s/sandboxmgr"
	"github.com/easy-platform/easyops/k8s/servicesmgr"
)

// TestMapK8sErr checks the shared k8s -> Connect code mapping.
func TestMapK8sErr(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	cases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"nil", nil, 0},
		{"forbidden", apierrors.NewForbidden(gr, "x", errors.New("no rbacs")), connect.CodePermissionDenied},
		{"unauthorized", apierrors.NewUnauthorized("nope"), connect.CodeUnauthenticated},
		{"notfound", apierrors.NewNotFound(gr, "x"), connect.CodeNotFound},
		{"alreadyexists", apierrors.NewAlreadyExists(gr, "x"), connect.CodeAlreadyExists},
		{"conflict", apierrors.NewConflict(gr, "x", errors.New("conflict")), connect.CodeAborted},
		{"generic", errors.New("boom"), connect.CodeInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mapK8sErr(c.err)
			if c.err == nil {
				if got != nil {
					t.Fatalf("nil error -> %v, want nil", got)
				}
				return
			}
			if connect.CodeOf(got) != c.want {
				t.Fatalf("code = %v, want %v", connect.CodeOf(got), c.want)
			}
		})
	}
}

// forbidList makes the fake clientset's LIST of `resource` fail with Forbidden
// (a missing worker-namespace Role/RoleBinding); everything else succeeds.
func forbidList(cs *fake.Clientset, resource string) {
	cs.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: resource}, "", errors.New("no matching Role/RoleBinding"))
	})
}

// forbiddenClientset returns a fake clientset whose LIST of `resource` fails
// with Forbidden.
func forbiddenClientset(resource string) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	forbidList(cs, resource)
	return cs
}

// TestListRPCsMapForbiddenToPermissionDenied checks that a k8s Forbidden from a
// list backend surfaces as CodePermissionDenied (403), NOT the old CodeInternal
// (500), on every list RPC.
func TestListRPCsMapForbiddenToPermissionDenied(t *testing.T) {
	ctx := context.Background()

	// sandboxes: pods list forbidden.
	sbx := sandboxmgr.NewWithClientset(forbiddenClientset("deployments"), sandboxmgr.Config{Namespace: "worker"})
	s := New(Deps{Sandbox: sbx, Runtime: runtimeprofiles.DefaultSettings()})
	if _, err := s.ListSandboxes(ctx, &opsv1.ListSandboxesRequest{Owner: "a"}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("ListSandboxes: code = %v, want PermissionDenied", connect.CodeOf(err))
	}

	// services: deployments list forbidden.
	svcs := servicesmgr.NewWithClientset(forbiddenClientset("deployments"), servicesmgr.Config{Namespace: "worker"})
	s2 := New(Deps{Sandbox: sbx, Runtime: runtimeprofiles.DefaultSettings(), Services: svcs})
	if _, err := s2.ListServices(ctx, &opsv1.ListServicesRequest{Owner: "a"}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("ListServices: code = %v, want PermissionDenied", connect.CodeOf(err))
	}

	// pvcs: persistentvolumeclaims list forbidden.
	pvcs := servicesmgr.NewWithClientset(forbiddenClientset("persistentvolumeclaims"), servicesmgr.Config{Namespace: "worker"})
	s3 := New(Deps{Sandbox: sbx, Runtime: runtimeprofiles.DefaultSettings(), Services: pvcs})
	if _, err := s3.ListPVCs(ctx, &opsv1.ListPVCsRequest{Owner: "a"}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("ListPVCs: code = %v, want PermissionDenied", connect.CodeOf(err))
	}
}

// TestForbiddenIsNotInternal is a regression guard: the old one-size-fits-all
// CodeInternal must not come back for a permission error.
func TestForbiddenIsNotInternal(t *testing.T) {
	ctx := context.Background()
	sbx := sandboxmgr.NewWithClientset(forbiddenClientset("deployments"), sandboxmgr.Config{Namespace: "worker"})
	s := New(Deps{Sandbox: sbx, Runtime: runtimeprofiles.DefaultSettings()})
	_, err := s.ListSandboxes(ctx, &opsv1.ListSandboxesRequest{Owner: "a"})
	if connect.CodeOf(err) == connect.CodeInternal {
		t.Fatal("Forbidden must not map to CodeInternal (500)")
	}
}
