// Package runtimeprofiles renders a workload's runtime capabilities. The
// caller picks only two orthogonal knobs — KVM on/off and a GPU count — and the
// gateway maps them to device requests, mounts, caps and resources. Callers
// never supply raw pod settings, so the security boundary is the deployment's
// Settings (overridable via a JSON env file).
package runtimeprofiles

import (
	"encoding/json"
	"os"

	corev1 "k8s.io/api/core/v1"
)

// Settings holds the deployment's runtime knobs.
type Settings struct {
	// KVMDevice is the extended resource the device plugin exposes for /dev/kvm.
	KVMDevice string `json:"kvmDevice"`
	// GPUDevice is the extended resource for NVIDIA GPUs.
	GPUDevice string `json:"gpuDevice"`
	// KVMNeedsTun mounts /dev/net/tun (VMs/tunnels need it).
	KVMNeedsTun bool `json:"kvmNeedsTun"`
	// DeviceResources maps a GENERIC device name (a caller-facing alias such as
	// "uinput", "tun", "drm") to the extended resource the cluster's generic
	// device plugin exposes for it (e.g. "squat.ai/uinput"). Requesting the
	// resource makes the plugin inject the device node via CDI — no hostPath.
	DeviceResources map[string]string `json:"deviceResources"`
	// KVMDevices are generic devices a KVM workload needs, requested via the
	// device plugin on every KVM render (e.g. ["tun"] to replace the historical
	// hostPath /dev/net/tun). Empty by default -> KVM falls back to the hostPath
	// tun mount (KVMNeedsTun).
	KVMDevices []string `json:"kvmDevices"`
	// KVMSecurityContext applied to a KVM container (privileged is ALWAYS
	// forced false by Render).
	KVMSecurityContext *corev1.SecurityContext `json:"kvmSecurityContext"`
	// GPURuntimeClass is the pod RuntimeClass that injects the NVIDIA driver
	// devices (nvidia-container-runtime). Without it a GPU request allocates
	// the device but /dev/nvidia* is NOT mounted. Empty = no runtime class.
	GPURuntimeClass string `json:"gpuRuntimeClass"`
	// KVMRuntimeClass is an optional RuntimeClass for KVM pods (the device
	// plugin usually suffices, so this is empty by default).
	KVMRuntimeClass string `json:"kvmRuntimeClass"`
}

// DefaultSettings are the built-in knobs (matching the cluster's device plugin
// and the agent-worker VM profiles).
func DefaultSettings() Settings {
	return Settings{
		KVMDevice:   "squat.ai/kvm",
		GPUDevice:   "nvidia.com/gpu",
		KVMNeedsTun: true,
		// The cluster's generic-device-plugin exposes these aliases.
		DeviceResources: map[string]string{
			"uinput": "squat.ai/uinput",
			"tun":    "squat.ai/tun",
			"drm":    "squat.ai/drm",
		},
		// KVM gets /dev/net/tun through the device plugin (squat.ai/tun) rather
		// than a hostPath CharDevice mount — the plugin injects the node via
		// CDI. Set kvmDevices=[] (or override) to fall back to the hostPath tun.
		KVMDevices: []string{"tun"},
		KVMSecurityContext: &corev1.SecurityContext{
			RunAsUser:                int64Ptr(0),
			AllowPrivilegeEscalation: boolPtr(true),
			Privileged:               boolPtr(false),
			// NOTE: NO SYS_ADMIN. KVM guests boot and configure NAT/bridge
			// networking with only NET_ADMIN + MKNOD (verified against the
			// deployment's macOS / Windows / Android images: QEMU boots, NAT
			// networking comes up, the worker serves, jobs run). SYS_ADMIN was a
			// conservative upstream default that also enables container escape
			// (mount/unshare/cgroup) on a shared node, so it is intentionally
			// dropped.
			Capabilities: &corev1.Capabilities{Add: []corev1.Capability{
				"NET_ADMIN", "NET_RAW", "SYS_NICE", "MKNOD",
				"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "FOWNER", "KILL",
			}},
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
			SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		},
		// The cluster's GPU device plugin installs an `nvidia` RuntimeClass
		// (nvidia-container-runtime); GPU pods must set it for the driver
		// devices to appear.
		GPURuntimeClass: "nvidia",
	}
}

// Load layers a JSON override file (env WORKSPACE_RUNTIME) over the defaults.
// A missing path yields the defaults.
func Load(path string) (Settings, error) {
	s := DefaultSettings()
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	var o Settings
	if err := json.Unmarshal(b, &o); err != nil {
		return s, err
	}
	if o.KVMDevice != "" {
		s.KVMDevice = o.KVMDevice
	}
	if o.GPUDevice != "" {
		s.GPUDevice = o.GPUDevice
	}
	if o.KVMNeedsTun {
		s.KVMNeedsTun = true
	}
	if len(o.DeviceResources) > 0 {
		if s.DeviceResources == nil {
			s.DeviceResources = map[string]string{}
		}
		for k, v := range o.DeviceResources {
			s.DeviceResources[k] = v
		}
	}
	if o.KVMDevices != nil {
		s.KVMDevices = o.KVMDevices
	}
	if o.KVMSecurityContext != nil {
		s.KVMSecurityContext = o.KVMSecurityContext
	}
	// A non-empty override replaces the default runtime class name.
	if o.GPURuntimeClass != "" {
		s.GPURuntimeClass = o.GPURuntimeClass
	}
	if o.KVMRuntimeClass != "" {
		s.KVMRuntimeClass = o.KVMRuntimeClass
	}
	return s, nil
}

// Rendered is the pod-level result.
type Rendered struct {
	NeedsTun     bool
	DeviceLimits map[string]string
	// Devices are the generic device aliases rendered via the device plugin
	// (informational; the actual injection is driven by DeviceLimits).
	Devices          []string
	NodeSelector     map[string]string
	ImagePullSecrets []string
	Env              map[string]string
	SecurityContext  *corev1.SecurityContext
	Resources        *corev1.ResourceRequirements
	// RuntimeClass selects the pod runtime handler. GPU pods need the NVIDIA
	// class so the driver devices are injected; KVM pods may set one too.
	RuntimeClass string
}

// Render maps (kvm, gpuCount) to pod settings. gpuCount > 0 requests that many
// GPUs. The returned SecurityContext always has Privileged=false.
func (s Settings) Render(kvm bool, gpuCount int) Rendered {
	r := Rendered{}
	if gpuCount > 0 {
		r.DeviceLimits = map[string]string{s.GPUDevice: itoa(gpuCount)}
		// Without the NVIDIA runtime class the device is allocated but the
		// driver devices (/dev/nvidia*) are never mounted.
		r.RuntimeClass = s.GPURuntimeClass
	}
	if kvm {
		if r.DeviceLimits == nil {
			r.DeviceLimits = map[string]string{}
		}
		r.DeviceLimits[s.KVMDevice] = "1"
		// Prefer the device plugin for KVM's generic devices when configured
		// (KVMDevices, e.g. ["tun"]); otherwise fall back to the hostPath tun.
		if len(s.KVMDevices) > 0 {
			r.Devices = append(r.Devices, s.KVMDevices...)
			for _, name := range s.KVMDevices {
				if res := s.DeviceResources[name]; res != "" {
					r.DeviceLimits[res] = "1"
				}
			}
		} else {
			r.NeedsTun = s.KVMNeedsTun
		}
		if r.RuntimeClass == "" {
			r.RuntimeClass = s.KVMRuntimeClass
		}
		if s.KVMSecurityContext != nil {
			sc := *s.KVMSecurityContext
			priv := false
			sc.Privileged = &priv
			r.SecurityContext = &sc
		}
	}
	return r
}

// DeviceLimitsFor maps generic device aliases (e.g. ["uinput"]) to the
// extended-resource limits the device plugin needs (e.g. {"squat.ai/uinput":
// "1"}). Unknown aliases are skipped. This lets a caller request arbitrary
// generic devices (uinput, drm, ...) in addition to KVM/GPU, without ever
// touching a hostPath.
func (s Settings) DeviceLimitsFor(devices []string) map[string]string {
	if len(devices) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, name := range devices {
		if res := s.DeviceResources[name]; res != "" {
			out[res] = "1"
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func int64Ptr(i int64) *int64 { return &i }
func boolPtr(b bool) *bool    { return &b }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
