// Package jobmgr runs run-to-completion workloads as Kubernetes Jobs
// (batch/v1) in the managed namespace — a one-shot buildctl pod, a migration,
// a test run. Unlike a Service (long-lived Deployment) a Job is expected to
// finish; unlike a Sandbox (a worker you talk to) it has no Service.
package jobmgr

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/easy-platform/easyops/k8s/k8sconfig"
)

// Labels/annotations stamped on every Job this manager owns.
const (
	LabelManaged  = "workspace/job"
	LabelName     = "workspace/job-name"
	AnnoImage     = "workspace/job-image"
	AnnoCreator   = "workspace/job-creator"
	AnnoSession   = "workspace/job-session"
	AnnoNamespace = "workspace/job-namespace"
)

// ErrExists reports that a job with the requested name already exists.
var ErrExists = fmt.Errorf("job already exists")

// VolumeMount binds an existing PVC into the job container.
type VolumeMount struct {
	Name      string
	Claim     string
	MountPath string
	SubPath   string
	ReadOnly  bool
}

// Spec is one job to create.
type Spec struct {
	Namespace string
	Name      string
	Image     string
	Command   []string
	Args      []string
	Env       map[string]string
	CPU       string
	Memory    string
	// RestartPolicy is "Never" (default) or "OnFailure".
	RestartPolicy string
	// BackoffLimit caps retries. nil => 0 for Never, 6 for OnFailure.
	BackoffLimit *int32
	// ActiveDeadlineSeconds bounds the whole job (0 = none).
	ActiveDeadlineSeconds int32
	// TTLSecondsAfterFinished auto-deletes a finished job (0 = keep).
	TTLSecondsAfterFinished int32
	Volumes                 []VolumeMount
	NodeSelector            map[string]string
	// ServiceAccountName, when set, runs the pod under that SA.
	ServiceAccountName string
	// Owner/Session are stamped as annotations (List filters by owner).
	Owner   string
	Session string
}

// Job is a live job view.
type Job struct {
	Name       string
	Namespace  string
	Image      string
	State      string // active | succeeded | failed
	Done       bool
	Succeeded  int32
	Failed     int32
	Active     int32
	Reason     string
	Message    string
	Creator    string
	Session    string
	CreatedAt  int64
	FinishedAt int64
	ExitCode   int32
}

// Config tunes the manager's defaults.
type Config struct {
	Namespace     string
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

// Client manages Jobs in one namespace.
type Client struct {
	cs        kubernetes.Interface
	namespace string
	cfg       Config
}

// New builds a Client from the in-cluster config.
func New(cfg Config) (*Client, error) {
	rc, err := k8sconfig.RESTConfig()
	if err != nil {
		return nil, fmt.Errorf("jobmgr: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("jobmgr: clientset: %w", err)
	}
	return NewWithClientset(cs, cfg), nil
}

// NewWithClientset is the test/DI seam.
func NewWithClientset(cs kubernetes.Interface, cfg Config) *Client {
	ns := cfg.Namespace
	if ns == "" {
		ns = "worker"
	}
	return &Client{cs: cs, namespace: ns, cfg: cfg}
}

// Namespace reports the namespace this client manages.
func (c *Client) Namespace() string { return c.namespace }

// Create creates the Job. ErrExists when one with that name is already present.
func (c *Client) Create(ctx context.Context, s Spec) (*Job, error) {
	if s.Name == "" {
		return nil, fmt.Errorf("jobmgr: name is required")
	}
	if s.Image == "" {
		return nil, fmt.Errorf("jobmgr: image is required")
	}
	ns := c.namespace
	if s.Namespace != "" {
		ns = s.Namespace
	}
	policy := corev1.RestartPolicyNever
	if strings.EqualFold(s.RestartPolicy, "OnFailure") {
		policy = corev1.RestartPolicyOnFailure
	}
	backoff := int32(0)
	if policy == corev1.RestartPolicyOnFailure {
		backoff = 6
	}
	if s.BackoffLimit != nil {
		backoff = *s.BackoffLimit
	}

	env := make([]corev1.EnvVar, 0, len(s.Env))
	for k, v := range s.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}

	container := corev1.Container{
		Name:    "job",
		Image:   s.Image,
		Command: s.Command,
		Args:    s.Args,
		Env:     env,
	}
	if s.CPU != "" || s.Memory != "" {
		r := corev1.ResourceRequirements{Limits: corev1.ResourceList{}, Requests: corev1.ResourceList{}}
		if s.CPU != "" {
			if q, err := resource.ParseQuantity(s.CPU); err == nil {
				r.Limits[corev1.ResourceCPU] = q
				r.Requests[corev1.ResourceCPU] = q
			}
		} else {
			for _, v := range []string{c.cfg.CPURequest, c.cfg.CPULimit} {
				if v == "" {
					continue
				}
				if q, err := resource.ParseQuantity(v); err == nil {
					r.Requests[corev1.ResourceCPU] = q
					r.Limits[corev1.ResourceCPU] = q
				}
				break
			}
		}
		if s.Memory != "" {
			if q, err := resource.ParseQuantity(s.Memory); err == nil {
				r.Limits[corev1.ResourceMemory] = q
				r.Requests[corev1.ResourceMemory] = q
			}
		}
		container.Resources = r
	}
	for _, v := range s.Volumes {
		if v.Claim == "" || v.MountPath == "" {
			continue
		}
		name := v.Name
		if name == "" {
			name = "v-" + strings.ReplaceAll(v.Claim, ".", "-")
		}
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: name, MountPath: v.MountPath, SubPath: v.SubPath, ReadOnly: v.ReadOnly,
		})
	}

	vols := make([]corev1.Volume, 0, len(s.Volumes))
	for _, v := range s.Volumes {
		if v.Claim == "" || v.MountPath == "" {
			continue
		}
		name := v.Name
		if name == "" {
			name = "v-" + strings.ReplaceAll(v.Claim, ".", "-")
		}
		vols = append(vols, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: v.Claim, ReadOnly: v.ReadOnly,
				},
			},
		})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: ns,
			Labels:    map[string]string{LabelManaged: "1", LabelName: s.Name},
			Annotations: map[string]string{
				AnnoImage: s.Image, AnnoCreator: s.Owner, AnnoSession: s.Session,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{LabelManaged: "1", LabelName: s.Name},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      policy,
					Containers:         []corev1.Container{container},
					Volumes:            vols,
					NodeSelector:       s.NodeSelector,
					ServiceAccountName: s.ServiceAccountName,
				},
			},
		},
	}
	if s.ActiveDeadlineSeconds > 0 {
		d := int64(s.ActiveDeadlineSeconds)
		job.Spec.ActiveDeadlineSeconds = &d
	}
	if s.TTLSecondsAfterFinished > 0 {
		t := s.TTLSecondsAfterFinished
		job.Spec.TTLSecondsAfterFinished = &t
	}
	created, err := c.cs.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil, ErrExists
	}
	if err != nil {
		return nil, err
	}
	return c.view(ctx, created), nil
}

// List returns the jobs owned by `owner` (empty owner = all managed jobs).
func (c *Client) List(ctx context.Context, owner string) ([]Job, error) {
	list, err := c.cs.BatchV1().Jobs(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1"})
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(list.Items))
	for i := range list.Items {
		j := &list.Items[i]
		if owner != "" && j.Annotations[AnnoCreator] != owner {
			continue
		}
		out = append(out, *c.view(ctx, j))
	}
	return out, nil
}

// Get returns one job.
func (c *Client) Get(ctx context.Context, name string) (*Job, error) {
	j, err := c.cs.BatchV1().Jobs(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return c.view(ctx, j), nil
}

// Delete removes a job and its pods.
func (c *Client) Delete(ctx context.Context, name, propagation string) error {
	pol := metav1.DeletePropagationBackground
	if strings.EqualFold(propagation, "foreground") {
		pol = metav1.DeletePropagationForeground
	}
	err := c.cs.BatchV1().Jobs(c.namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &pol})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("job not found: %s", name)
	}
	return err
}

// view maps a k8s Job to the manager's view, resolving the state, the first
// failing container's reason and the pod's exit code (best effort).
func (c *Client) view(ctx context.Context, j *batchv1.Job) *Job {
	v := &Job{
		Name: j.Name, Namespace: j.Namespace,
		Image:     j.Annotations[AnnoImage],
		Creator:   j.Annotations[AnnoCreator],
		Session:   j.Annotations[AnnoSession],
		CreatedAt: j.CreationTimestamp.UnixMilli(),
	}
	if j.Spec.Template.Spec.Containers != nil && v.Image == "" {
		v.Image = j.Spec.Template.Spec.Containers[0].Image
	}
	if j.Status.Succeeded > 0 {
		v.Succeeded = j.Status.Succeeded
	}
	if j.Status.Failed > 0 {
		v.Failed = j.Status.Failed
	}
	if j.Status.Active > 0 {
		v.Active = j.Status.Active
	}
	// State: succeeded > failed > active.
	switch {
	case j.Status.Succeeded > 0:
		v.State, v.Done = "succeeded", true
	case j.Status.Failed > 0 && j.Status.Active == 0:
		v.State, v.Done = "failed", true
	default:
		v.State = "active"
	}
	if c := jobCondition(j, batchv1.JobComplete); c != nil {
		v.Done = true
		v.State = "succeeded"
		if c.LastTransitionTime.Time.UnixMilli() > 0 {
			v.FinishedAt = c.LastTransitionTime.UnixMilli()
		}
	}
	if c := jobCondition(j, batchv1.JobFailed); c != nil {
		v.Done = true
		v.State = "failed"
		v.Reason = c.Reason
		v.Message = c.Message
		if c.LastTransitionTime.Time.UnixMilli() > 0 {
			v.FinishedAt = c.LastTransitionTime.UnixMilli()
		}
	}
	// Pod diagnostics: the first unhealthy container explains a stuck job.
	pod := c.jobPod(ctx, j.Name)
	if pod != nil {
		if pod.Status.Phase == corev1.PodFailed && v.State != "failed" {
			v.State, v.Done = "failed", true
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil {
				v.ExitCode = t.ExitCode
				if t.ExitCode != 0 && v.Reason == "" {
					v.Reason, v.Message = t.Reason, t.Message
				}
			}
			if w := cs.State.Waiting; w != nil && w.Reason != "" && v.Reason == "" {
				v.Reason, v.Message = w.Reason, w.Message
			}
		}
		if v.FinishedAt == 0 && pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
			if len(pod.Status.ContainerStatuses) > 0 {
				if t := pod.Status.ContainerStatuses[0].State.Terminated; t != nil {
					v.FinishedAt = t.FinishedAt.UnixMilli()
				}
			}
		}
	}
	return v
}

func jobCondition(j *batchv1.Job, t batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range j.Status.Conditions {
		if j.Status.Conditions[i].Type == t && j.Status.Conditions[i].Status == corev1.ConditionTrue {
			return &j.Status.Conditions[i]
		}
	}
	return nil
}

// jobPod returns the newest pod for a job (nil when none exists yet).
func (c *Client) jobPod(ctx context.Context, name string) *corev1.Pod {
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelName + "=" + name})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}
	newest := &pods.Items[0]
	for i := range pods.Items {
		if pods.Items[i].CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = &pods.Items[i]
		}
	}
	return newest
}

// Logs reads a job pod's logs (tail).
func (c *Client) Logs(ctx context.Context, name, container string, tail int64) (string, error) {
	pod := c.jobPod(ctx, name)
	if pod == nil {
		return "", fmt.Errorf("no pod for job %s (not scheduled yet)", name)
	}
	opts := &corev1.PodLogOptions{Container: container}
	if tail > 0 {
		opts.TailLines = &tail
	}
	rc, err := c.cs.CoreV1().Pods(c.namespace).GetLogs(pod.Name, opts).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var b strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return b.String(), nil
}

// Wait settles until the job is done or the deadline passes.
func (c *Client) Wait(ctx context.Context, name string, timeout time.Duration) (*Job, error) {
	deadline := time.Now().Add(timeout)
	for {
		j, err := c.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		if j.Done || time.Now().After(deadline) {
			return j, nil
		}
		select {
		case <-ctx.Done():
			return j, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
