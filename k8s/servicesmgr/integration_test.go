package servicesmgr

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestServiceLifecycle is the horizontal test for servicesmgr: a Deployment +
// Service that really serves traffic, survives a scale/pause/resume, and is
// removed cleanly.
//
// A fake clientset cannot decide any of this: whether the container starts,
// whether the Service targetPort maps to the container port, whether a readiness
// probe is honoured, or whether Delete leaves a Service behind.
func TestServiceLifecycle(t *testing.T) {
	c := newClusterClient(t)
	ctx := context.Background()
	name := fmt.Sprintf("svc-%d", time.Now().UnixNano())
	img := fixtureImage(t, "web:1")

	if _, err := c.Deploy(ctx, Spec{
		Name: name, Image: img,
		ContainerPort: 80,
		Creator:       "alice",
		Session:       "sess-1",
		Namespace:     "team-a",
		ReadinessProbe: Probe{
			HTTPPath: "/", HTTPPort: 80,
			PeriodSeconds: 2, FailureThreshold: 30,
		},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// 1. It becomes ready (the container starts and the probe passes).
	ready := waitReady(t, c, name, 3*time.Minute)
	if ready.Name != name {
		t.Errorf("name = %q", ready.Name)
	}
	if ready.Creator != "alice" || ready.Session != "sess-1" {
		t.Errorf("creator/session not carried: %+v", ready)
	}

	// 2. Reachable THROUGH the Service from inside the cluster. This is the only
	//    assertion that proves the port mapping (Service 80 -> container 80).
	body := runInCluster(t, c, img, "wget -q -T 5 -O - http://"+name+"/")
	if !strings.Contains(strings.ToLower(body), "nginx") {
		t.Errorf("service response = %q", body)
	}

	// 3. Get / List / Manifest observe the live object.
	if _, err := c.Get(ctx, name); err != nil {
		t.Fatalf("get: %v", err)
	}
	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var seen bool
	for _, s := range list {
		if s.Name == name {
			seen = true
		}
	}
	if !seen {
		t.Error("deployed service missing from List")
	}
	m, err := c.Manifest(ctx, name)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if !strings.Contains(m, "kind: Deployment") {
		t.Errorf("manifest does not look like a Deployment:\n%s", m)
	}

	// 4. Scale up to 2 and observe two ready replicas.
	if _, err := c.Scale(ctx, name, 2); err != nil {
		t.Fatalf("scale: %v", err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		s, err := c.Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if s.ReadyReplicas == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scale to 2 never became ready (replicas=%d ready=%d)", s.Replicas, s.ReadyReplicas)
		}
		time.Sleep(time.Second)
	}

	// 5. Pause scales to 0 (and remembers 2); Resume restores it.
	paused, err := c.Pause(ctx, name)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !paused.Paused || paused.Replicas != 0 {
		t.Errorf("paused = %+v, want Paused=true Replicas=0", paused)
	}
	resumed, err := c.Resume(ctx, name)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Paused || resumed.Replicas != 2 {
		t.Errorf("resumed = %+v, want Paused=false Replicas=2", resumed)
	}

	// 6. A second Deploy is idempotent (it updates in place, same name).
	if _, err := c.Deploy(ctx, Spec{Name: name, Image: img, ContainerPort: 80, Creator: "alice", Session: "sess-1"}); err != nil {
		t.Fatalf("idempotent deploy: %v", err)
	}

	// 7. Delete removes the Deployment AND its Service.
	ok, err := c.Delete(ctx, name)
	if err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if _, err := c.Get(ctx, name); err == nil {
		t.Error("the Deployment still exists after Delete")
	}
	svcs, err := c.cs.CoreV1().Services(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManaged + "=1," + LabelName + "=" + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs.Items) != 0 {
		t.Errorf("the Service was left behind: %v", svcs.Items)
	}
	if ok, err := c.Delete(ctx, name); err != nil || ok {
		t.Errorf("a second Delete must be a no-op: ok=%v err=%v", ok, err)
	}
}

// TestServiceRollbackRestoresThePreviousImage is the one servicesmgr operation
// whose correctness is entirely in the cluster: it copies a ReplicaSet's pod
// template back onto the Deployment. A fake cannot prove the RS history exists.
func TestServiceRollbackRestoresThePreviousImage(t *testing.T) {
	c := newClusterClient(t)
	ctx := context.Background()
	name := fmt.Sprintf("svc-rb-%d", time.Now().UnixNano())
	img := fixtureImage(t, "web:1")

	// Revision 1: web.
	if _, err := c.Deploy(ctx, Spec{Name: name, Image: img, ContainerPort: 80, Creator: "alice"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	defer func() { _, _ = c.Delete(ctx, name) }()
	waitReady(t, c, name, 3*time.Minute)

	// Revision 2: the same image with a marker env var, so the rollback target
	// is distinguishable.
	marker := "rev-2"
	if _, err := c.Deploy(ctx, Spec{
		Name: name, Image: img, ContainerPort: 80, Creator: "alice",
		Env: map[string]string{"MARKER": marker},
	}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		s, err := c.Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if s.Revision >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("revision never advanced past 1 (got %d)", s.Revision)
		}
		time.Sleep(time.Second)
	}

	// Roll back to the previous revision and assert the marker is gone from the
	// live Deployment (i.e. the OLD pod template is back).
	after, err := c.Rollback(ctx, name, 0)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if after.Revision < 3 {
		t.Errorf("a rollback must create a new revision, got %d", after.Revision)
	}
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "MARKER" {
			t.Fatalf("rollback did not restore the previous pod template (MARKER still present)")
		}
	}
}

// TestServicePortConflictIsRefused pins the tenant-safety precheck: Deploy must
// never clobber a Service it does not own.
func TestServicePortConflictIsRefused(t *testing.T) {
	c := newClusterClient(t)
	ctx := context.Background()
	name := fmt.Sprintf("svc-conflict-%d", time.Now().UnixNano())

	// Stage a foreign Service with the name Deploy would use.
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.namespace},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Name: "p", Port: 80}},
		},
	}
	if _, err := c.cs.CoreV1().Services(c.namespace).Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatalf("stage foreign service: %v", err)
	}
	defer func() {
		_ = c.cs.CoreV1().Services(c.namespace).Delete(context.Background(), name, metav1.DeleteOptions{})
	}()

	_, err := c.Deploy(ctx, Spec{
		Name: name, Image: fixtureImage(t, "web:1"), ContainerPort: 80, Creator: "alice",
	})
	if err == nil {
		t.Fatal("Deploy must refuse to overwrite a Service it does not own")
	}
	if !strings.Contains(err.Error(), "not managed") {
		t.Errorf("error = %v, want a 'not managed by this tenant' error", err)
	}
}

// TestServiceCannotPullReportsReason: a service whose image cannot be pulled
// must explain itself instead of silently never becoming ready.
func TestServiceCannotPullReportsReason(t *testing.T) {
	c := newClusterClient(t)
	ctx := context.Background()
	name := fmt.Sprintf("svc-bad-%d", time.Now().UnixNano())

	if _, err := c.Deploy(ctx, Spec{
		Name: name, Image: fixtureImage(t, "nope:1"), ContainerPort: 80, Creator: "alice",
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	defer func() { _, _ = c.Delete(ctx, name) }()

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		s, err := c.Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(s.Message, "ImagePullBackOff") || strings.Contains(s.Message, "ErrImagePull") {
			return // diagnosed
		}
		time.Sleep(2 * time.Second)
	}
	s, _ := c.Get(ctx, name)
	t.Fatalf("an unpullable service must report a reason (phase=%q msg=%q)", s.PodPhase, s.Message)
}
