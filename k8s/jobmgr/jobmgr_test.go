package jobmgr

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newClient() (*Client, *fake.Clientset) {
	cs := fake.NewSimpleClientset()
	return NewWithClientset(cs, Config{Namespace: "dev"}), cs
}

// TestCreateJobBuildsTheObject pins the object the manager renders: labels,
// annotations (owner/session), restart policy, backoff, env, volumes and the
// optional deadline/ttl.
func TestCreateJobBuildsTheObject(t *testing.T) {
	c, cs := newClient()
	bl := int32(3)
	j, err := c.Create(context.Background(), Spec{
		Name: "build1", Image: "registry/img:tag",
		Command: []string{"/bin/sh"}, Args: []string{"-c", "echo hi"},
		Env:                     map[string]string{"A": "1"},
		RestartPolicy:           "OnFailure",
		BackoffLimit:            &bl,
		ActiveDeadlineSeconds:   60,
		TTLSecondsAfterFinished: 120,
		Owner:                   "myuser",
		Session:                 "s1",
		Volumes:                 []VolumeMount{{Claim: "data", MountPath: "/data", ReadOnly: true}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if j.Name != "build1" || j.Namespace != "dev" {
		t.Errorf("job = %+v", j)
	}

	got, err := cs.BatchV1().Jobs("dev").Get(context.Background(), "build1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Annotations[AnnoCreator] != "myuser" {
		t.Errorf("creator annotation = %q", got.Annotations[AnnoCreator])
	}
	if got.Annotations[AnnoSession] != "s1" {
		t.Errorf("session annotation = %q", got.Annotations[AnnoSession])
	}
	if got.Labels[LabelManaged] != "1" {
		t.Errorf("managed label = %q", got.Labels[LabelManaged])
	}
	pod := got.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Errorf("restartPolicy = %q", pod.RestartPolicy)
	}
	if got.Spec.BackoffLimit == nil || *got.Spec.BackoffLimit != 3 {
		t.Errorf("backoffLimit = %v", got.Spec.BackoffLimit)
	}
	if got.Spec.ActiveDeadlineSeconds == nil || *got.Spec.ActiveDeadlineSeconds != 60 {
		t.Errorf("activeDeadline = %v", got.Spec.ActiveDeadlineSeconds)
	}
	if got.Spec.TTLSecondsAfterFinished == nil || *got.Spec.TTLSecondsAfterFinished != 120 {
		t.Errorf("ttl = %v", got.Spec.TTLSecondsAfterFinished)
	}
	c0 := pod.Containers[0]
	if c0.Image != "registry/img:tag" || len(c0.Command) != 1 || len(c0.Args) != 2 {
		t.Errorf("container = %+v", c0)
	}
	if len(c0.Env) != 1 || c0.Env[0].Name != "A" {
		t.Errorf("env = %+v", c0.Env)
	}
	if len(c0.VolumeMounts) != 1 || c0.VolumeMounts[0].MountPath != "/data" || !c0.VolumeMounts[0].ReadOnly {
		t.Errorf("mounts = %+v", c0.VolumeMounts)
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].PersistentVolumeClaim == nil || pod.Volumes[0].PersistentVolumeClaim.ClaimName != "data" {
		t.Errorf("volumes = %+v", pod.Volumes)
	}
}

// TestCreateJobDefaults: Never + backoff 0 when unset; a missing name/image is
// an error.
func TestCreateJobDefaults(t *testing.T) {
	c, cs := newClient()
	if _, err := c.Create(context.Background(), Spec{Name: "j", Image: "i"}); err != nil {
		t.Fatal(err)
	}
	got, _ := cs.BatchV1().Jobs("dev").Get(context.Background(), "j", metav1.GetOptions{})
	if got.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("default restartPolicy = %q", got.Spec.Template.Spec.RestartPolicy)
	}
	if got.Spec.BackoffLimit == nil || *got.Spec.BackoffLimit != 0 {
		t.Errorf("default backoff = %v", got.Spec.BackoffLimit)
	}
	if _, err := c.Create(context.Background(), Spec{Image: "i"}); err == nil {
		t.Error("missing name must error")
	}
	if _, err := c.Create(context.Background(), Spec{Name: "x"}); err == nil {
		t.Error("missing image must error")
	}
}

// TestCreateJobExists: a second create with the same name is ErrExists.
func TestCreateJobExists(t *testing.T) {
	c, _ := newClient()
	if _, err := c.Create(context.Background(), Spec{Name: "dup", Image: "i"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(context.Background(), Spec{Name: "dup", Image: "i"}); err != ErrExists {
		t.Errorf("second create = %v, want ErrExists", err)
	}
}

// TestJobStateFromStatus: the view derives state/done from the Job's counters
// and, on failure, surfaces the pod's terminated reason + exit code.
func TestJobStateFromStatus(t *testing.T) {
	c, cs := newClient()
	ctx := context.Background()

	// Succeeded.
	ok := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "ok", Namespace: "dev", Labels: map[string]string{LabelManaged: "1", LabelName: "ok"}, Annotations: map[string]string{AnnoImage: "i", AnnoCreator: "u"}},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
	_, _ = cs.BatchV1().Jobs("dev").Create(ctx, ok, metav1.CreateOptions{})
	v, err := c.Get(ctx, "ok")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "succeeded" || !v.Done {
		t.Errorf("ok view = %+v", v)
	}

	// Failed via the Job's Failed condition + a terminated pod with exit 2.
	failed := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "dev", Labels: map[string]string{LabelManaged: "1", LabelName: "bad"}, Annotations: map[string]string{AnnoImage: "i", AnnoCreator: "u"}},
		Status: batchv1.JobStatus{
			Failed: 1,
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
			}},
		},
	}
	_, _ = cs.BatchV1().Jobs("dev").Create(ctx, failed, metav1.CreateOptions{})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-abc", Namespace: "dev", Labels: map[string]string{LabelName: "bad"}},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "job",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 2, Reason: "Error",
				}},
			}},
		},
	}
	_, _ = cs.CoreV1().Pods("dev").Create(ctx, pod, metav1.CreateOptions{})

	v, err = c.Get(ctx, "bad")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "failed" || !v.Done {
		t.Errorf("bad view = %+v", v)
	}
	if v.Reason == "" {
		t.Errorf("failure reason is empty: %+v", v)
	}
	if v.ExitCode != 2 {
		t.Errorf("exitCode = %d, want 2", v.ExitCode)
	}
}

// TestListFiltersByOwner: List returns only the caller's jobs.
func TestListFiltersByOwner(t *testing.T) {
	c, _ := newClient()
	ctx := context.Background()
	for _, s := range []Spec{
		{Name: "a", Image: "i", Owner: "mine"},
		{Name: "b", Image: "i", Owner: "theirs"},
	} {
		if _, err := c.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	mine, err := c.List(ctx, "mine")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Name != "a" {
		t.Errorf("list(mine) = %+v", mine)
	}
	all, _ := c.List(ctx, "")
	if len(all) != 2 {
		t.Errorf("list(all) = %d, want 2", len(all))
	}
}

// TestDeleteJob removes the object; a missing job is an error.
func TestDeleteJob(t *testing.T) {
	c, _ := newClient()
	ctx := context.Background()
	if _, err := c.Create(ctx, Spec{Name: "d", Image: "i"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "d", ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Get(ctx, "d"); err == nil {
		t.Error("job still present after delete")
	}
	if err := c.Delete(ctx, "nope", ""); err == nil {
		t.Error("deleting a missing job must error")
	}
}
