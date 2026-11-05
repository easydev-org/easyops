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

	appsv1 "k8s.io/api/apps/v1"
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
	LabelJob      = "workspace/job-owner"
	LabelService  = "workspace/job-service"
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
	// SecretEnv binds environment variables to KEYS of existing secrets.
	//
	// It exists so a CI job can receive a credential WITHOUT the value ever
	// entering the Job spec (and therefore the apiserver's object store or any
	// log): the kubelet injects it from the Secret at container start.
	SecretEnv []SecretEnvVar
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
	// Tenant is the caller's tenant-internal namespace. It is stamped as a
	// LABEL (never used as the Kubernetes namespace: the server pins that from
	// its own config, or a caller could run a container anywhere in the
	// cluster).
	Tenant string
	// Untrusted marks the job as running UNTRUSTED code (the CI case). It forces
	// automountServiceAccountToken=false, a non-root, read-only-rootfs,
	// no-capabilities security context, and forbids `privileged`. A job that
	// runs a tenant's arbitrary script must never be able to talk to the
	// Kubernetes API with the namespace's ServiceAccount.
	Untrusted bool
	// Services are companion containers started alongside the job (a database,
	// a cache). See ServiceSpec.
	Services []ServiceSpec
	// Workspace, when set, is mounted at /workspace: a PVC claim name for cache
	// reuse, or "" for an ephemeral emptyDir.
	Workspace string
}

// ServiceSpec is one companion container for a job.
//
// It is created as a single-replica Deployment + Service (so the job can resolve
// it by name) and labelled with the job so a cleanup deletes both.
// SecretEnvVar binds one environment variable to a key of a Secret.
type SecretEnvVar struct {
	Env    string
	Secret string
	Key    string
}

type ServiceSpec struct {
	// Name is the hostname the job uses (a DNS label).
	Name    string
	Image   string
	Env     map[string]string
	Ports   []int32
	Command []string
	Args    []string
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
	// UntrustedUID / UntrustedGID are the UID/GID an UNTRUSTED (CI) job runs as.
	// They default to 65532 (the distroless "nonroot" id).
	//
	// They are required, not optional: `runAsNonRoot: true` WITHOUT an explicit
	// UID makes kubelet refuse to start any image whose USER is root (which is
	// most of them) with CreateContainerConfigError. An explicit UID is what
	// makes the guarantee enforceable rather than a deployment trap.
	UntrustedUID int64
	UntrustedGID int64
	// ServiceReadyTimeout bounds how long a job waits for its companion services
	// to become addressable before the Job is created (0 = don't wait).
	//
	// It is NOT optional in practice: a job created at the same instant as its
	// companion starts before the Service has endpoints and DNS answers, so the
	// job's first connection fails with "name not resolvable". The integration
	// suite caught exactly that. Waiting is the only deterministic fix; the image
	// pull is what makes the companion take a few seconds.
	ServiceReadyTimeout time.Duration
}

// DefaultUntrustedID is the UID/GID untrusted jobs run as when Config leaves it
// unset (65532 = the distroless `nonroot` user, which exists in most images as a
// plain numeric id even when it has no passwd entry).
const DefaultUntrustedID int64 = 65532

// untrustedIDs resolves the UID/GID for untrusted jobs.
func (c Config) untrustedIDs() (int64, int64) {
	uid, gid := c.UntrustedUID, c.UntrustedGID
	if uid == 0 {
		uid = DefaultUntrustedID
	}
	if gid == 0 {
		gid = uid
	}
	return uid, gid
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

	env := make([]corev1.EnvVar, 0, len(s.Env)+len(s.SecretEnv))
	for k, v := range s.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	// Secret-backed env: a REFERENCE, so the value never reaches this process.
	for _, se := range s.SecretEnv {
		if se.Env == "" || se.Secret == "" || se.Key == "" {
			return nil, fmt.Errorf("jobmgr: secret env needs env/secret/key, got %+v", se)
		}
		env = append(env, corev1.EnvVar{Name: se.Env, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: se.Secret},
				Key:                  se.Key,
			},
		}})
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
				AnnoNamespace: s.Tenant,
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

	// A job that runs UNTRUSTED code (CI) must never receive the namespace's
	// ServiceAccount token, must not run as root, must not write to its root
	// filesystem and must not gain capabilities. Without this, any tenant's
	// workflow could talk to the Kubernetes API as the easyops ServiceAccount.
	if s.Untrusted {
		if s.ServiceAccountName != "" {
			// An explicitly requested SA would hand the token back; refuse it.
			return nil, fmt.Errorf("jobmgr: untrusted job %q must not set service_account", s.Name)
		}
		f := false
		tr := true
		nonRoot := true
		uid, gid := c.cfg.untrustedIDs()
		job.Spec.Template.Spec.AutomountServiceAccountToken = &f
		container.SecurityContext = &corev1.SecurityContext{
			AllowPrivilegeEscalation: &f,
			Privileged:               &f,
			ReadOnlyRootFilesystem:   &tr,
			RunAsNonRoot:             &nonRoot,
			// An explicit numeric UID is mandatory here: without it, kubelet
			// refuses every image whose USER is root — which is most of them —
			// and the job dies with CreateContainerConfigError instead of running.
			RunAsUser:  &uid,
			RunAsGroup: &gid,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}
		// A read-only root filesystem still needs writable scratch space for the
		// checkout, the toolchain's caches and the step output file.
		container.VolumeMounts = append(container.VolumeMounts,
			corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"},
			corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"},
		)
		vols = append(vols,
			corev1.Volume{Name: "tmp", VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			}},
			workspaceVolume(s.Workspace),
		)
		job.Spec.Template.Spec.Volumes = vols
		job.Spec.Template.Spec.Containers = []corev1.Container{container}
		// Pod-level IDs: the FSGroup makes the emptyDir volumes group-writable,
		// which is what lets a non-root container actually use /workspace and
		// /tmp (an emptyDir is root-owned 0755 by default).
		job.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{
			RunAsNonRoot:   &nonRoot,
			RunAsUser:      &uid,
			RunAsGroup:     &gid,
			FSGroup:        &gid,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}
	}
	if s.ActiveDeadlineSeconds > 0 {
		d := int64(s.ActiveDeadlineSeconds)
		job.Spec.ActiveDeadlineSeconds = &d
	}
	if s.TTLSecondsAfterFinished > 0 {
		t := s.TTLSecondsAfterFinished
		job.Spec.TTLSecondsAfterFinished = &t
	}

	// Companion services MUST exist (and have a ready endpoint) BEFORE the Job
	// is created. A job that starts at the same instant as its database cannot
	// resolve the Service name yet and its first connection fails immediately —
	// the k3d integration suite caught exactly that ("NO_DNS" / BackoffLimit).
	// Creating them first also means a failed companion setup fails the Create
	// call instead of leaving a job that is doomed to fail.
	if err := c.createServices(ctx, ns, s); err != nil {
		return nil, err
	}
	if err := c.waitServicesReady(ctx, s); err != nil {
		return nil, err
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

// createServices creates one Deployment + Service per companion, labelled with
// the job name so Delete removes them together.
func (c *Client) createServices(ctx context.Context, ns string, s Spec) error {
	for _, svc := range s.Services {
		if svc.Name == "" || svc.Image == "" {
			continue
		}
		labels := map[string]string{
			LabelManaged: "1", LabelJob: s.Name, LabelService: svc.Name,
		}
		replicas := int32(1)
		env := make([]corev1.EnvVar, 0, len(svc.Env))
		for k, v := range svc.Env {
			env = append(env, corev1.EnvVar{Name: k, Value: v})
		}
		ports := make([]corev1.ContainerPort, 0, len(svc.Ports))
		for _, p := range svc.Ports {
			ports = append(ports, corev1.ContainerPort{ContainerPort: p})
		}
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: s.Name + "-" + svc.Name, Namespace: ns, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						AutomountServiceAccountToken: boolPtr(false),
						Containers: []corev1.Container{{
							Name: svc.Name, Image: svc.Image, Env: env,
							Command: svc.Command, Args: svc.Args, Ports: ports,
						}},
					},
				},
			},
		}
		if _, err := c.cs.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("jobmgr: service %s: %w", svc.Name, err)
		}
		k8sSvc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: ns, Labels: labels},
			Spec: corev1.ServiceSpec{
				Selector: labels,
				Ports:    servicePorts(svc.Ports),
			},
		}
		if _, err := c.cs.CoreV1().Services(ns).Create(ctx, k8sSvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("jobmgr: service %s: %w", svc.Name, err)
		}
	}
	return nil
}

// waitServicesReady blocks until every companion Service has at least one ready
// endpoint, so the job's first connection cannot lose a race with the image pull.
//
// A timeout is NOT an error: the companion may be slow (a large image), and
// failing the whole job would be worse than letting it retry. The caller's own
// readiness/retry logic still applies; this only removes the common race.
func (c *Client) waitServicesReady(ctx context.Context, s Spec) error {
	if len(s.Services) == 0 {
		return nil
	}
	timeout := c.cfg.ServiceReadyTimeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for _, svc := range s.Services {
		if svc.Name == "" {
			continue
		}
		if err := c.waitOneServiceReady(ctx, s.Namespace, svc.Name, deadline); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) waitOneServiceReady(ctx context.Context, ns, name string, deadline time.Time) error {
	if ns == "" {
		ns = c.namespace
	}
	for {
		eps, err := c.cs.CoreV1().Endpoints(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			ready := 0
			for _, sub := range eps.Subsets {
				ready += len(sub.Addresses)
			}
			if ready > 0 {
				return nil
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("jobmgr: service %s endpoints: %w", name, err)
		}
		if time.Now().After(deadline) {
			return nil // best effort: do not fail the job on a slow companion
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func servicePorts(in []int32) []corev1.ServicePort {
	out := make([]corev1.ServicePort, 0, len(in))
	for _, p := range in {
		out = append(out, corev1.ServicePort{Name: fmt.Sprintf("p%d", p), Port: p})
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

// EnsureWorkspacePVC creates the cache claim a job mounts at /workspace, when
// it does not exist yet.
//
// It is idempotent so a caller can call it before every run: the first run
// provisions the volume, later runs reuse whatever the toolchain cached.
func (c *Client) EnsureWorkspacePVC(ctx context.Context, name, size string) error {
	if name == "" {
		return fmt.Errorf("jobmgr: pvc name is required")
	}
	if size == "" {
		size = "1Gi"
	}
	_, err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil // already there: reuse the cache
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("jobmgr: get pvc %s: %w", name, err)
	}
	q, qerr := resource.ParseQuantity(size)
	if qerr != nil {
		return fmt.Errorf("jobmgr: bad pvc size %q: %w", size, qerr)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.namespace,
			Labels:    map[string]string{LabelManaged: "1"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: q},
			},
		},
	}
	if _, err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // lost a race with another run: fine, it exists
		}
		return fmt.Errorf("jobmgr: create pvc %s: %w", name, err)
	}
	return nil
}

// DeleteWorkspacePVC removes a cache claim (missing is not an error).
func (c *Client) DeleteWorkspacePVC(ctx context.Context, name string) error {
	err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// workspaceVolume returns the /workspace volume for a job: a PVC claim when
// Workspace is set (cache reuse across runs), else an ephemeral emptyDir.
func workspaceVolume(claim string) corev1.Volume {
	if claim == "" {
		return corev1.Volume{Name: "workspace", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}}
	}
	return corev1.Volume{Name: "workspace", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
	}}
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
	// Companion services are labelled with the job name, so they go with it.
	// (ServiceInterface has no DeleteCollection, so list and delete one by one.)
	sel := metav1.ListOptions{LabelSelector: LabelJob + "=" + name}
	if list, err := c.cs.CoreV1().Services(c.namespace).List(ctx, sel); err == nil {
		for i := range list.Items {
			_ = c.cs.CoreV1().Services(c.namespace).Delete(ctx, list.Items[i].Name, metav1.DeleteOptions{})
		}
	}
	if list, err := c.cs.AppsV1().Deployments(c.namespace).List(ctx, sel); err == nil {
		prop := metav1.DeletePropagationBackground
		for i := range list.Items {
			_ = c.cs.AppsV1().Deployments(c.namespace).Delete(ctx, list.Items[i].Name,
				metav1.DeleteOptions{PropagationPolicy: &prop})
		}
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
