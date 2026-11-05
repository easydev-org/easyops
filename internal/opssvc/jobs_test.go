package opssvc

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/k8s/jobmgr"
)

// TestCreateJobCannotChooseAKubernetesNamespace is a tenant-escape regression
// test.
//
// CreateJob used to pass req.namespace straight into jobmgr.Spec.Namespace, which
// OVERRIDES the manager's own namespace. Any caller could therefore run a
// container in any namespace of the cluster. The server must pin the namespace
// from its own configuration and treat req.namespace as an opaque tenant label.
func TestCreateJobCannotChooseAKubernetesNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset()
	jm := jobmgr.NewWithClientset(cs, jobmgr.Config{Namespace: "worker"})
	svc := New(Deps{Jobs: jm})

	_, err := svc.CreateJob(context.Background(), &opsv1.CreateJobRequest{
		Owner: "alice", Name: "escape", Namespace: "kube-system",
		Image: "busybox:1.36", Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// A Job must exist in the configured namespace...
	if _, err := cs.BatchV1().Jobs("worker").Get(context.Background(), "escape", metav1.GetOptions{}); err != nil {
		t.Fatalf("the job was not created in the configured namespace: %v", err)
	}
	// ...and NONE in the namespace the caller asked for.
	if _, err := cs.BatchV1().Jobs("kube-system").Get(context.Background(), "escape", metav1.GetOptions{}); err == nil {
		t.Fatal("a caller must not be able to place a Job in another namespace")
	}
}

// TestCreateJobRejectsServiceAccount: a job is untrusted code and must never be
// handed a token through an explicit ServiceAccount.
func TestCreateJobRejectsServiceAccount(t *testing.T) {
	cs := fake.NewSimpleClientset()
	jm := jobmgr.NewWithClientset(cs, jobmgr.Config{Namespace: "worker"})
	svc := New(Deps{Jobs: jm})

	_, err := svc.CreateJob(context.Background(), &opsv1.CreateJobRequest{
		Owner: "alice", Name: "tokenized", Image: "busybox:1.36",
		ServiceAccount: []string{"builder"},
	})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
	}
}
