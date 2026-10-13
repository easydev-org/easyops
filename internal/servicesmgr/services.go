// Package servicesmgr manages long-lived services: a user image run as a
// Kubernetes Deployment + Service in the managed namespace. Unlike sandboxes,
// services get NO worker injection and NO sidecar — they are the caller's app.
package servicesmgr

import (
	"context"
	"fmt"

	"github.com/easy-platform/easyops/internal/k8sconfig"
	"github.com/easy-platform/easyops/internal/runtimeprofiles"
	"k8s.io/client-go/kubernetes"
)

// Labels/annotations stamped on every service object.
const (
	LabelManaged = "workspace/service"
	LabelName    = "workspace/service-name"
	AnnoImage    = "workspace/service-image"
	AnnoCreator  = "workspace/service-creator"
	// AnnoSession binds a service to the session that deployed it, so the webui
	// can show which session owns it (services stay tenant-visible).
	AnnoSession = "workspace/service-session"
	// AnnoNamespace records the tenant-internal namespace (org segment of the
	// creating session) for the tenant-internal read/write split.
	AnnoNamespace = "workspace/service-namespace"
	// AnnoReplicasBeforePause remembers the replica count in effect when a
	// service was paused (scaled to 0), so Resume can restore it. Presence of
	// this annotation is what marks a service as PAUSED.
	AnnoReplicasBeforePause = "workspace/service-replicas-before-pause"
	// PVC labels/annotations stamped on every claim this manager creates.
	LabelPVC       = "workspace/pvc"
	LabelPVCName   = "workspace/pvc-name"
	AnnoPVCCreator = "workspace/pvc-creator"
	// AnnoPVCNamespace records the tenant-internal namespace of a claim.
	AnnoPVCNamespace = "workspace/pvc-namespace"
)

// VolumeMount binds a named PVC into a service container.
type VolumeMount struct {
	PVC       string
	MountPath string
	ReadOnly  bool
	SubPath   string
}

// Service is a live service view.
type Service struct {
	Name     string
	Image    string
	Phase    string
	Ready    bool
	Replicas int32
	URL      string
	Creator  string
	Session  string
	// Namespace is the tenant-internal namespace (org segment of the session).
	Namespace string
	CreatedAt int64
	// Ports is every port this service exposes (across its primary Service and
	// any sibling `<name>-<suffix>` Services).
	Ports []Port
	// Paused is true when the service was scaled to zero by Pause (Replicas
	// then reads 0). Resume restores the pre-pause replica count.
	Paused bool
	// Pod diagnostics (best-effort; zero when no pod is observed).
	PodPhase string
	Restarts int32
	// Message is the waiting/terminated reason of the first unhealthy container
	// (e.g. CrashLoopBackOff, ImagePullBackOff); empty when healthy.
	Message string
	// ReadyReplicas is how many pods report ready (from the Deployment status).
	ReadyReplicas int32
	// CPU / Memory are the container's resource requests as k8s quantity
	// strings ("" when unset).
	CPU    string
	Memory string
	// Command is the container argv override (nil = the image default).
	Command []string
	// Env is the container's environment variables.
	Env map[string]string
	// Volumes are the PVCs mounted into the container.
	Volumes []VolumeMount
	// ---- Tier 0 detail (observed from the live Deployment) ----
	ConfigMounts   []ConfigMount
	Resources      Resources
	ReadinessProbe Probe
	LivenessProbe  Probe
	StartupProbe   Probe
	Rollout        Rollout
	SidecarCount   int32
	// Revision is the Deployment's current rollout revision (the target of
	// RollbackService). 0 when the Deployment has not rolled out yet.
	Revision int64
}

// LogOptions selects which container log to read.
type LogOptions struct {
	// TailLines limits the returned lines (0 = all available in the buffer).
	TailLines int64
	// Previous reads the PREVIOUS container instance (the crash that restarted
	// it); useful for CrashLoopBackOff.
	Previous bool
	// Follow keeps streaming new lines until the stream ends (watch mode).
	Follow bool
}

// LogSource reads a service's container logs. It is a seam so a future
// centralized log backend (e.g. Loki) can be added without touching the RPC or
// webui. The default implementation reads the Kubernetes pod log directly.
type LogSource interface {
	// Tail returns up to opts.TailLines lines (Follow=false).
	Tail(ctx context.Context, name string, opts LogOptions) ([]string, error)
	// Follow streams lines until ctx is cancelled or the container exits.
	Follow(ctx context.Context, name string, opts LogOptions) (<-chan string, <-chan error)
}

// Port is one exposed port of a service.
type Port struct {
	// Suffix selects the k8s Service: "" = the primary `<name>`; else the
	// sibling `<name>-<suffix>`. Ports sharing a suffix live in one Service.
	Suffix string
	// Port is the k8s Service port (80 or 443).
	Port int32
	// Protocol is "tcp" or "udp".
	Protocol string
	// TargetPort is the container port to forward to.
	TargetPort int32
}

// PresetPorts maps a preset name to its (port, protocol).
func PresetPorts(preset string) (port int32, proto string, ok bool) {
	switch preset {
	case "tcp80":
		return 80, "tcp", true
	case "tcp443":
		return 443, "tcp", true
	case "udp443":
		return 443, "udp", true
	}
	return 0, "", false
}

// Probe is a container probe. Exactly one of HTTPGet/TCP/Exec should be set;
// HTTPGet wins over TCP over Exec.
type Probe struct {
	HTTPPath            string
	HTTPPort            int32
	TCPPort             int32
	Exec                []string
	InitialDelaySeconds int32
	PeriodSeconds       int32
	TimeoutSeconds      int32
	FailureThreshold    int32
	SuccessThreshold    int32
}

// Empty reports whether the probe has no action (so it should be omitted).
func (p Probe) Empty() bool {
	return p.HTTPPort == 0 && p.TCPPort == 0 && len(p.Exec) == 0
}

// Resources splits requests from limits (each field optional).
type Resources struct {
	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string
}

// Empty reports whether no request/limit was given.
func (r Resources) Empty() bool {
	return r.CPURequest == "" && r.MemoryRequest == "" && r.CPULimit == "" && r.MemoryLimit == ""
}

// EnvRef injects one env var from a ConfigMap/Secret key.
type EnvRef struct {
	Name      string
	ConfigMap string
	ConfigKey string
	Secret    string
	SecretKey string
}

// EnvFrom injects all keys of a ConfigMap or Secret as env vars.
type EnvFrom struct {
	ConfigMap string
	Secret    string
}

// ConfigMount mounts a ConfigMap or Secret as a volume.
type ConfigMount struct {
	ConfigMap string
	Secret    string
	MountPath string
	Items     []KeyToPath
}

// KeyToPath selects one key of a ConfigMap/Secret into a relative file path.
type KeyToPath struct {
	Key  string
	Path string
}

// Sidecar is an extra container in the pod (Init=true => an init container).
type Sidecar struct {
	Name    string
	Image   string
	Command []string
	Env     map[string]string
	CPU     string
	Memory  string
	Init    bool
}

// Toleration is one pod toleration.
type Toleration struct {
	Key      string
	Operator string
	Value    string
	Effect   string
}

// Rollout is the Deployment's RollingUpdate strategy.
type Rollout struct {
	MaxSurge       string
	MaxUnavailable string
}

// Spec describes a service to deploy.
type Spec struct {
	Name          string
	Image         string
	Command       []string
	Env           map[string]string
	CPU           string
	Memory        string
	Replicas      int32
	ContainerPort int32
	ServicePort   int32
	Creator       string
	// Session is the session that deployed the service (empty = unbound).
	Session string
	// Namespace is the tenant-internal namespace (org segment of the session).
	Namespace string
	// Ports are the resolved ports to expose. Empty = the default single public
	// port (tcp80 -> ContainerPort). Entries sharing a Suffix form one Service.
	Ports []Port
	// Runtime is the rendered runtime profile (device limits, security context,
	// tun mount, node selector, ...). Zero value = a plain service.
	Runtime runtimeprofiles.Rendered
	// Volumes are named PVCs to mount into the container.
	Volumes []VolumeMount
	// ---- Tier 0 enhancements (all optional) ----
	Resources      Resources
	ReadinessProbe Probe
	LivenessProbe  Probe
	StartupProbe   Probe
	Rollout        Rollout
	EnvRefs        []EnvRef
	EnvFrom        []EnvFrom
	ConfigMounts   []ConfigMount
	Sidecars       []Sidecar
	NodeSelector   map[string]string
	Tolerations    []Toleration
}

// Client wraps the typed clientset plus the target namespace.
type Client struct {
	cs        kubernetes.Interface
	namespace string
	res       resDefaults
}

// resources holds the deployment's default service request/limit values.
type resDefaults struct {
	cpuReq, cpuLim, memReq, memLim string
}

// Config configures a Client.
type Config struct {
	Namespace string
	// Default resource REQUESTS/LIMITS for a service container that omits
	// cpu/memory. Requests default to 500m / 1Gi; limits to 2 / 4Gi (Burstable).
	// An explicit cpu/memory on the service still sets request == limit.
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
		return nil, fmt.Errorf("servicesmgr: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("servicesmgr: clientset: %w", err)
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
		res: resDefaults{
			cpuReq: cfg.CPURequest, cpuLim: cfg.CPULimit,
			memReq: cfg.MemoryRequest, memLim: cfg.MemoryLimit,
		},
	}
}

// Namespace returns the managed namespace.
func (c *Client) Namespace() string { return c.namespace }
