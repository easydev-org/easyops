// Package sandboxmgr is the gateway's Kubernetes backend: it creates, lists,
// resolves and deletes agent-worker sandboxes as Pod + Service + Secret triples
// in a single namespace. It knows nothing about repos or CI.
package sandboxmgr

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/easy-platform/easyops/internal/k8sconfig"
	"github.com/easy-platform/easyops/internal/runtimeprofiles"
	"k8s.io/client-go/kubernetes"
)

// Labels/annotations the manager stamps on every object it owns.
const (
	LabelManaged = "worker-manager/sandbox"
	LabelName    = "worker-manager/name"
	AnnoImage    = "worker-manager/image"
	AnnoCreator  = "worker-manager/creator"
	// AnnoSession binds a sandbox to the session that created it (its owner),
	// so a session can enumerate and fan out to its own sandboxes.
	AnnoSession = "worker-manager/session"

	// WorkerPort is the agent-worker listen port baked into preset images.
	WorkerPort = 48080
	// ServicePort is the port the sandbox Service EXPOSES. The worker listens on
	// WorkerPort (48080); the Service fronts it on 80 so callers use the plain
	// `http://<sandbox>.<ns>.svc.cluster.local` form (and the edge/desktop wiring
	// that maps a sandbox's port 80 works).
	ServicePort = 80
)

// ErrExists reports that a sandbox with the requested name already exists. A
// sandbox name is never reused: creating over a live one would silently destroy
// its workload and token, so callers must delete it first.
var ErrExists = errors.New("sandbox already exists")

// Sandbox is a live worker view.
type Sandbox struct {
	Name      string
	Image     string
	Phase     string
	Ready     bool
	URL       string
	Creator   string
	Session   string
	CreatedAt int64 // unix millis
	// Pod diagnostics (best-effort): total container restarts and the first
	// unhealthy container's reason+message (e.g. `OOMKilled`, `CrashLoopBackOff`,
	// `RunContainerError`). Empty when healthy.
	Restarts int32
	Message  string
	// More diagnostics (best-effort) for the first unhealthy container, so a
	// Failed/Pending sandbox explains itself: exit code + terminated reason,
	// lastState reason, the container name, and the container's start/finish
	// times (unix millis; 0 = unset).
	ExitCode         int32
	TerminatedReason string
	LastStateReason  string
	Container        string
	StartedAt        int64
	FinishedAt       int64
}

// Spec describes a sandbox to create.
type Spec struct {
	Name    string
	Image   string
	CPU     string
	Memory  string
	Env     map[string]string
	Creator string
	// Session binds the sandbox to its owning session (empty = unbound).
	Session string
	// Runtime is the rendered runtime capabilities (device limits, security
	// context, tun mount, ...). Zero value = a plain sandbox.
	Runtime runtimeprofiles.Rendered
	// GoldenDiskPVC, when non-empty, mounts that PVC (read-write) at
	// GoldenDiskCachePath so a VM sandbox's golden-disk base is cached once and
	// shared across sandboxes. Empty = no cache volume.
	GoldenDiskPVC string
	// Volumes are extra PVCs to mount into the worker container (tenant-owned).
	Volumes []VolumeMount
}

// VolumeMount is one PVC mounted into a sandbox container.
type VolumeMount struct {
	PVC       string
	MountPath string
	ReadOnly  bool
	SubPath   string
}

// GoldenDiskCachePath is where golden-disk.sh caches the base qcow2 (its
// GOLDEN_DISK_CACHE default). A VM sandbox mounts the shared cache PVC here.
const GoldenDiskCachePath = "/storage/.cache"

// Client wraps the typed clientset plus the target namespace.
type Client struct {
	cs        kubernetes.Interface
	namespace string
	res       resources
}

// resources holds the deployment's default sandbox request/limit values.
type resources struct {
	cpuReq, cpuLim, memReq, memLim string
}

// orDefault returns v when non-empty, else def.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Config configures a Client.
type Config struct {
	Namespace string
	// Default resource REQUESTS/LIMITS applied when a sandbox omits cpu/memory.
	// Requests default to 500m / 1Gi; limits default to 2 / 4Gi (so a sandbox
	// is Burstable: it reserves little but may burst). An explicit cpu/memory on
	// the sandbox still sets request == limit.
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

// New builds a client from the shared REST config (see internal/k8sconfig):
// EASYOPS_KUBECONFIG is REQUIRED, so a deployment can run in one namespace but
// act with another's identity.
func New(cfg Config) (*Client, error) {
	rc, err := k8sconfig.RESTConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("k8s: clientset: %w", err)
	}
	return NewWithClientset(cs, cfg), nil
}

// NewWithClientset is the test/DI seam.
func NewWithClientset(cs kubernetes.Interface, cfg Config) *Client {
	ns := cfg.Namespace
	if ns == "" {
		ns = "worker"
	}
	return &Client{
		cs: cs, namespace: ns,
		res: resources{
			cpuReq: cfg.CPURequest, cpuLim: cfg.CPULimit,
			memReq: cfg.MemoryRequest, memLim: cfg.MemoryLimit,
		},
	}
}

// Namespace returns the namespace the manager operates in.
func (c *Client) Namespace() string { return c.namespace }

var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// resourceName maps a logical sandbox name to a DNS-1123 object name, hashing
// when the input is not already valid (so arbitrary names never collide).
func resourceName(name string) string {
	base := strings.ToLower(name)
	if len(base) <= 40 && nameRe.MatchString(base) {
		return "wm-" + base
	}
	sum := sha256.Sum256([]byte(name))
	return "wm-" + hex.EncodeToString(sum[:])[:16]
}

// newToken mints a 32-byte hex bearer token.
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func defaultResources(cpu, mem string, d resources) corev1.ResourceRequirements {
	req := corev1.ResourceList{}
	lim := corev1.ResourceList{}
	if cpu != "" {
		// Explicit cpu/memory keep request == limit.
		q := resource.MustParse(cpu)
		req[corev1.ResourceCPU] = q
		lim[corev1.ResourceCPU] = q
	} else {
		req[corev1.ResourceCPU] = resource.MustParse(orDefault(d.cpuReq, "500m"))
		lim[corev1.ResourceCPU] = resource.MustParse(orDefault(d.cpuLim, "2"))
	}
	if mem != "" {
		q := resource.MustParse(mem)
		req[corev1.ResourceMemory] = q
		lim[corev1.ResourceMemory] = q
	} else {
		req[corev1.ResourceMemory] = resource.MustParse(orDefault(d.memReq, "1Gi"))
		lim[corev1.ResourceMemory] = resource.MustParse(orDefault(d.memLim, "4Gi"))
	}
	return corev1.ResourceRequirements{Requests: req, Limits: lim}
}

// ServiceDNS returns the in-cluster Service URL for a sandbox.
func (c *Client) ServiceDNS(resource string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", resource, c.namespace, ServicePort)
}

// Create creates the Pod + Service + Secret for a sandbox and returns the
// sandbox plus its bearer token. It does NOT wait for readiness (the caller
// does, via WaitReady).
func (c *Client) Create(ctx context.Context, s Spec) (Sandbox, string, error) {
	if s.Name == "" || s.Image == "" {
		return Sandbox{}, "", fmt.Errorf("name and image required")
	}
	res := resourceName(s.Name)
	token, err := newToken()
	if err != nil {
		return Sandbox{}, "", err
	}

	labels := map[string]string{
		LabelManaged: "1",
		LabelName:    s.Name,
	}
	annotations := map[string]string{
		AnnoImage:   s.Image,
		AnnoCreator: s.Creator,
		AnnoSession: s.Session,
	}

	// A name is NEVER reused: creating over an existing sandbox would silently
	// destroy a running workload (and its token). The K8s Create calls below are
	// the authoritative guard — an already-present object yields ErrExists and we
	// leave the existing sandbox untouched.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: res, Namespace: c.namespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte(token)},
	}
	if _, err := c.cs.CoreV1().Secrets(c.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return Sandbox{}, "", fmt.Errorf("%w: %s", ErrExists, s.Name)
		}
		return Sandbox{}, "", fmt.Errorf("create secret: %w", err)
	}

	env := []corev1.EnvVar{
		{Name: "WORKER_TOKEN", Value: token},
		{Name: "WORKER_PORT", Value: fmt.Sprintf("%d", WorkerPort)},
	}
	for k, v := range s.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}

	// Runtime profile: extra env, devices, caps, resources, tun mount.
	for k, v := range s.Runtime.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	resources := defaultResources(s.CPU, s.Memory, c.res)
	if s.Runtime.Resources != nil {
		resources = *s.Runtime.Resources
	}
	for k, v := range s.Runtime.DeviceLimits {
		if resources.Limits == nil {
			resources.Limits = corev1.ResourceList{}
		}
		resources.Limits[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	container := corev1.Container{
		Name:            "worker",
		Image:           s.Image,
		Env:             env,
		Ports:           []corev1.ContainerPort{{Name: "worker", ContainerPort: WorkerPort}},
		Resources:       resources,
		SecurityContext: s.Runtime.SecurityContext,
		// Always: a sandbox image tag (e.g. sandbox-macos:base) is REBUILT in
		// place when the guest disk or worker changes, and a long-lived tag is
		// otherwise served from the node's image cache — so an update would not
		// take effect. Always makes the kubelet re-resolve the tag on every pod.
		ImagePullPolicy: corev1.PullAlways,
	}
	podSpec := corev1.PodSpec{
		// A Deployment's pod template requires Always; the worker is a long-lived
		// server, and a crash is still surfaced via restart count + lastState.
		RestartPolicy: corev1.RestartPolicyAlways,
		Containers:    []corev1.Container{container},
	}
	if s.Runtime.RuntimeClass != "" {
		rc := s.Runtime.RuntimeClass
		podSpec.RuntimeClassName = &rc
	}
	if len(s.Runtime.NodeSelector) > 0 {
		podSpec.NodeSelector = s.Runtime.NodeSelector
	}
	for _, sec := range s.Runtime.ImagePullSecrets {
		podSpec.ImagePullSecrets = append(podSpec.ImagePullSecrets, corev1.LocalObjectReference{Name: sec})
	}
	if s.Runtime.NeedsTun {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: "devtun",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: "/dev/net/tun", Type: hostPathTypePtr(corev1.HostPathCharDev),
			}},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: "devtun", MountPath: "/dev/net/tun",
		})
	}
	if s.GoldenDiskPVC != "" {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: "golden-disk-cache",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: s.GoldenDiskPVC,
			}},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: "golden-disk-cache", MountPath: GoldenDiskCachePath,
		})
	}
	if len(s.Volumes) > 0 {
		for i, v := range s.Volumes {
			name := fmt.Sprintf("vol-%d", i)
			podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
				Name: name,
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: v.PVC,
					ReadOnly:  v.ReadOnly,
				}},
			})
			podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
				Name: name, MountPath: v.MountPath, ReadOnly: v.ReadOnly, SubPath: v.SubPath,
			})
		}
	}
	// A sandbox is a LONG-LIVED single-replica workload, so it is a Deployment
	// (never a bare Pod): the Deployment owns the pod, gives it a stable
	// identity, and self-heals a transient crash (a persistent failure is still
	// visible via the pod's restart count + lastState). A bare Pod is NOT
	// allowed anywhere in easyops.
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: res, Namespace: c.namespace, Labels: labels, Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec:       podSpec,
			},
		},
	}
	if _, err := c.cs.AppsV1().Deployments(c.namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		_ = c.cs.CoreV1().Secrets(c.namespace).Delete(ctx, res, metav1.DeleteOptions{})
		if apierrors.IsAlreadyExists(err) {
			return Sandbox{}, "", fmt.Errorf("%w: %s", ErrExists, s.Name)
		}
		return Sandbox{}, "", fmt.Errorf("create deployment: %w", err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: res, Namespace: c.namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name: "worker", Port: ServicePort, TargetPort: intstr.FromInt32(WorkerPort),
			}},
		},
	}
	if _, err := c.cs.CoreV1().Services(c.namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		_ = c.cs.AppsV1().Deployments(c.namespace).Delete(ctx, res, metav1.DeleteOptions{})
		_ = c.cs.CoreV1().Secrets(c.namespace).Delete(ctx, res, metav1.DeleteOptions{})
		if apierrors.IsAlreadyExists(err) {
			return Sandbox{}, "", fmt.Errorf("%w: %s", ErrExists, s.Name)
		}
		return Sandbox{}, "", fmt.Errorf("create service: %w", err)
	}

	sb := Sandbox{
		Name: s.Name, Image: s.Image, Phase: "Pending",
		URL: c.ServiceDNS(res), Creator: s.Creator, Session: s.Session,
		CreatedAt: time.Now().UnixMilli(),
	}
	return sb, token, nil
}

// Get returns one sandbox by logical name.
func (c *Client) Get(ctx context.Context, name string) (Sandbox, bool, error) {
	res := resourceName(name)
	// The sandbox is a Deployment; read its OWNED pod (created by the
	// Deployment's ReplicaSet).
	pod, err := c.depPod(ctx, res)
	if err != nil {
		return Sandbox{}, false, err
	}
	if pod != nil {
		return toSandbox(c, pod), true, nil
	}
	// No pod yet (still rolling out, or already reaped): fall back to the
	// Deployment's own metadata so owner/image/session are still reported.
	dep, derr := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, res, metav1.GetOptions{})
	if apierrors.IsNotFound(derr) {
		return Sandbox{}, false, nil
	}
	if derr != nil {
		return Sandbox{}, false, derr
	}
	return Sandbox{
		Name:      name,
		Image:     dep.Annotations[AnnoImage],
		Phase:     "Pending",
		URL:       c.ServiceDNS(res),
		Creator:   dep.Annotations[AnnoCreator],
		Session:   dep.Annotations[AnnoSession],
		CreatedAt: dep.CreationTimestamp.UnixMilli(),
	}, true, nil
}

// depPod returns the Deployment's pod (matched by the sandbox labels), or nil
// when the pod does not exist yet.
func (c *Client) depPod(ctx context.Context, res string) (*corev1.Pod, error) {
	list, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManaged + "=1",
	})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		if podOwnerDeployment(&list.Items[i]) == res {
			return &list.Items[i], nil
		}
	}
	// Fallback: the pod's name is prefixed by the Deployment name.
	for i := range list.Items {
		if strings.HasPrefix(list.Items[i].Name, res+"-") {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// podOwnerDeployment maps a pod to its owning Deployment name (pod -> ReplicaSet
// -> Deployment; the ReplicaSet name is the Deployment name + a hash suffix).
func podOwnerDeployment(p *corev1.Pod) string {
	for _, o := range p.OwnerReferences {
		if o.Kind == "ReplicaSet" {
			if i := strings.LastIndex(o.Name, "-"); i > 0 {
				return o.Name[:i]
			}
			return o.Name
		}
	}
	return ""
}

// List returns every sandbox the manager owns in its namespace.
func (c *Client) List(ctx context.Context) ([]Sandbox, error) {
	deps, err := c.cs.AppsV1().Deployments(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManaged + "=1",
	})
	if err != nil {
		return nil, err
	}
	out := make([]Sandbox, 0, len(deps.Items))
	for i := range deps.Items {
		d := &deps.Items[i]
		pod, perr := c.depPod(ctx, d.Name)
		if perr != nil || pod == nil {
			out = append(out, Sandbox{Name: d.Labels[LabelName], Image: d.Annotations[AnnoImage], Phase: "Pending", URL: c.ServiceDNS(d.Name), Creator: d.Annotations[AnnoCreator], Session: d.Annotations[AnnoSession], CreatedAt: d.CreationTimestamp.UnixMilli()})
			continue
		}
		out = append(out, toSandbox(c, pod))
	}
	return out, nil
}

// Delete removes the Pod + Service + Secret for a sandbox and waits (bounded)
// for the pod to actually disappear, so an immediate List does not still show
// it as Terminating.
func (c *Client) Delete(ctx context.Context, name string) (bool, error) {
	res := resourceName(name)
	// Only delete something we own (avoid nuking an unrelated object that
	// happens to share the name).
	if _, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, res, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := c.deleteObjects(ctx, res); err != nil {
		return false, err
	}
	// Wait for the Job to be gone (deletion is asynchronous; the Job's pod is
	// garbage-collected with it).
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, res, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return true, nil
}

// Resolve returns the URL + bearer token for a sandbox.
func (c *Client) Resolve(ctx context.Context, name string) (string, string, error) {
	res := resourceName(name)
	sec, err := c.cs.CoreV1().Secrets(c.namespace).Get(ctx, res, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", name, err)
	}
	token := string(sec.Data["token"])
	return c.ServiceDNS(res), token, nil
}

func (c *Client) deleteObjects(ctx context.Context, res string) error {
	_ = c.cs.CoreV1().Services(c.namespace).Delete(ctx, res, metav1.DeleteOptions{})
	_ = c.cs.CoreV1().Secrets(c.namespace).Delete(ctx, res, metav1.DeleteOptions{})
	// Foreground propagation so the Deployment's owned pod is deleted with it.
	prop := metav1.DeletePropagationForeground
	err := c.cs.AppsV1().Deployments(c.namespace).Delete(ctx, res, metav1.DeleteOptions{PropagationPolicy: &prop})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func hostPathTypePtr(t corev1.HostPathType) *corev1.HostPathType { return &t }

// Watch returns a watch over every sandbox Pod in the namespace. The caller
// re-lists on each signal (see k8swatch), so only the event stream matters.
func (c *Client) Watch(ctx context.Context) (watch.Interface, error) {
	return c.cs.CoreV1().Pods(c.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: LabelManaged + "=1",
	})
}
