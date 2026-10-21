package servicesmgr

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestRollbackToPreviousRevision deploys, then simulates a rollout history by
// planting two ReplicaSets, and checks Rollback(0) restores the previous
// revision's pod template onto the Deployment.
func TestRollbackToPreviousRevision(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, err := c.Deploy(ctx, Spec{Name: "app", Image: "img:2", Creator: "t", ContainerPort: 8080}); err != nil {
		t.Fatal(err)
	}
	// Plant revision history: rev1 = img:1, rev2 = img:2 (current).
	for _, rs := range []struct {
		name, rev, img string
	}{
		{"app-aaa", "1", "img:1"},
		{"app-bbb", "2", "img:2"},
	} {
		_, err := c.cs.AppsV1().ReplicaSets("worker").Create(ctx, &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: rs.name, Namespace: "worker",
				Labels:      map[string]string{"app": "app"},
				Annotations: map[string]string{"deployment.kubernetes.io/revision": rs.rev},
			},
			Spec: appsv1.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "svc", Image: rs.img}}},
				},
			},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Mark the Deployment as currently at rev2.
	d, _ := c.cs.AppsV1().Deployments("worker").Get(ctx, "app", metav1.GetOptions{})
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Annotations["deployment.kubernetes.io/revision"] = "2"
	if _, err := c.cs.AppsV1().Deployments("worker").Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	svc, err := c.Rollback(ctx, "app", 0)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := svc.Image; got != "img:1" {
		t.Fatalf("after rollback image = %q, want img:1", got)
	}
}

func TestRevisionsListsHistory(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, err := c.Deploy(ctx, Spec{Name: "app", Image: "img:1", Creator: "t"}); err != nil {
		t.Fatal(err)
	}
	_, err := c.cs.AppsV1().ReplicaSets("worker").Create(ctx, &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-aaa", Namespace: "worker",
			Labels:      map[string]string{"app": "app"},
			Annotations: map[string]string{"deployment.kubernetes.io/revision": "1"},
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "svc", Image: "img:1"}}}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	revs, err := c.Revisions(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 1 || revs[0].Revision != 1 || revs[0].Image != "img:1" {
		t.Fatalf("revisions = %+v", revs)
	}
}

var _ = fake.NewSimpleClientset
