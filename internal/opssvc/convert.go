package opssvc

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/k8s/servicesmgr"
)

// ---- proto -> servicesmgr conversions ----

// toVolumeMounts maps PVC volume specs to library mounts.
func toVolumeMounts(in []*opsv1.VolumeMountSpec) []servicesmgr.VolumeMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]servicesmgr.VolumeMount, 0, len(in))
	for _, v := range in {
		out = append(out, servicesmgr.VolumeMount{
			PVC: v.GetPvc(), MountPath: v.GetMountPath(),
			ReadOnly: v.GetReadOnly(), SubPath: v.GetSubPath(),
		})
	}
	return out
}

// toResources maps a split resource spec (nil-safe).
func toResources(r *opsv1.ResourceSpec) servicesmgr.Resources {
	if r == nil {
		return servicesmgr.Resources{}
	}
	return servicesmgr.Resources{
		CPURequest: r.GetCpu(), MemoryRequest: r.GetMemory(),
		CPULimit: r.GetCpuLimit(), MemoryLimit: r.GetMemoryLimit(),
	}
}

// toProbe maps a probe spec (nil-safe).
func toProbe(p *opsv1.ProbeSpec) servicesmgr.Probe {
	if p == nil {
		return servicesmgr.Probe{}
	}
	return servicesmgr.Probe{
		HTTPPath: p.GetHttpPath(), HTTPPort: p.GetHttpPort(), TCPPort: p.GetTcpPort(),
		Exec:                p.GetExecCommand(),
		InitialDelaySeconds: p.GetInitialDelaySeconds(), PeriodSeconds: p.GetPeriodSeconds(),
		TimeoutSeconds: p.GetTimeoutSeconds(), FailureThreshold: p.GetFailureThreshold(),
		SuccessThreshold: p.GetSuccessThreshold(),
	}
}

// toRollout maps a rollout spec (nil-safe).
func toRollout(r *opsv1.RolloutSpec) servicesmgr.Rollout {
	if r == nil {
		return servicesmgr.Rollout{}
	}
	return servicesmgr.Rollout{MaxSurge: r.GetMaxSurge(), MaxUnavailable: r.GetMaxUnavailable()}
}

// toEnvRefs maps ConfigMap/Secret env refs.
func toEnvRefs(in []*opsv1.EnvRefSpec) []servicesmgr.EnvRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]servicesmgr.EnvRef, 0, len(in))
	for _, e := range in {
		out = append(out, servicesmgr.EnvRef{
			Name: e.GetName(), ConfigMap: e.GetConfigMap(), ConfigKey: e.GetConfigKey(),
			Secret: e.GetSecret(), SecretKey: e.GetSecretKey(),
		})
	}
	return out
}

// toEnvFrom maps whole-config envFrom specs.
func toEnvFrom(in []*opsv1.EnvFromSpec) []servicesmgr.EnvFrom {
	if len(in) == 0 {
		return nil
	}
	out := make([]servicesmgr.EnvFrom, 0, len(in))
	for _, e := range in {
		out = append(out, servicesmgr.EnvFrom{ConfigMap: e.GetConfigMap(), Secret: e.GetSecret()})
	}
	return out
}

// toConfigMounts maps ConfigMap/Secret volume mounts.
func toConfigMounts(in []*opsv1.ConfigMountSpec) []servicesmgr.ConfigMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]servicesmgr.ConfigMount, 0, len(in))
	for _, c := range in {
		cm := servicesmgr.ConfigMount{
			ConfigMap: c.GetConfigMap(), Secret: c.GetSecret(), MountPath: c.GetMountPath(),
		}
		for _, it := range c.GetItems() {
			cm.Items = append(cm.Items, servicesmgr.KeyToPath{Key: it.GetKey(), Path: it.GetPath()})
		}
		out = append(out, cm)
	}
	return out
}

// toSidecars maps extra containers.
func toSidecars(in []*opsv1.SidecarSpec) []servicesmgr.Sidecar {
	if len(in) == 0 {
		return nil
	}
	out := make([]servicesmgr.Sidecar, 0, len(in))
	for _, sc := range in {
		out = append(out, servicesmgr.Sidecar{
			Name: sc.GetName(), Image: sc.GetImage(), Command: sc.GetCommand(),
			Env: sc.GetEnv(), CPU: sc.GetCpu(), Memory: sc.GetMemory(), Init: sc.GetInit(),
		})
	}
	return out
}

// toTolerations maps pod tolerations.
func toTolerations(in []*opsv1.TolerationSpec) []servicesmgr.Toleration {
	if len(in) == 0 {
		return nil
	}
	out := make([]servicesmgr.Toleration, 0, len(in))
	for _, t := range in {
		out = append(out, servicesmgr.Toleration{
			Key: t.GetKey(), Operator: t.GetOperator(), Value: t.GetValue(), Effect: t.GetEffect(),
		})
	}
	return out
}

// ---- servicesmgr -> proto conversions ----

// toServiceInfo maps a library Service to its wire form.
func toServiceInfo(svc servicesmgr.Service) *opsv1.ServiceInfo {
	info := &opsv1.ServiceInfo{
		Name: svc.Name, Image: svc.Image, Phase: svc.Phase, Ready: svc.Ready,
		Replicas: svc.Replicas, Url: svc.URL, Owner: svc.Creator, Session: svc.Session,
		Namespace: svc.Namespace,
		PodPhase:  svc.PodPhase, Restarts: svc.Restarts,
		Message: svc.Message, Paused: svc.Paused,
		CreatedAt: svc.CreatedAt, ReadyReplicas: svc.ReadyReplicas,
		Cpu: svc.CPU, Memory: svc.Memory, Command: svc.Command, Env: svc.Env,
		Resources:      fromResources(svc.Resources),
		ReadinessProbe: fromProbe(svc.ReadinessProbe),
		LivenessProbe:  fromProbe(svc.LivenessProbe),
		StartupProbe:   fromProbe(svc.StartupProbe),
		Rollout:        fromRollout(svc.Rollout),
		SidecarCount:   svc.SidecarCount,
		Revision:       svc.Revision,
	}
	for _, p := range svc.Ports {
		info.Ports = append(info.Ports, &opsv1.ServicePortInfo{
			Name: p.Suffix, Preset: presetFor(p.Port, p.Protocol),
			Port: p.Port, Protocol: p.Protocol, TargetPort: p.TargetPort,
		})
	}
	for _, v := range svc.Volumes {
		info.Volumes = append(info.Volumes, &opsv1.VolumeMountInfo{
			Pvc: v.PVC, MountPath: v.MountPath, ReadOnly: v.ReadOnly, SubPath: v.SubPath,
		})
	}
	for _, c := range svc.ConfigMounts {
		info.ConfigMounts = append(info.ConfigMounts, &opsv1.ConfigMountInfo{
			ConfigMap: c.ConfigMap, Secret: c.Secret, MountPath: c.MountPath,
		})
	}
	return info
}

// fromResources maps a split resource spec to the wire form (nil when empty).
func fromResources(r servicesmgr.Resources) *opsv1.ResourceSpec {
	if r.Empty() {
		return nil
	}
	return &opsv1.ResourceSpec{
		Cpu: r.CPURequest, Memory: r.MemoryRequest,
		CpuLimit: r.CPULimit, MemoryLimit: r.MemoryLimit,
	}
}

// fromProbe maps a probe to the wire form (nil when empty).
func fromProbe(p servicesmgr.Probe) *opsv1.ProbeSpec {
	if p.Empty() {
		return nil
	}
	return &opsv1.ProbeSpec{
		HttpPath: p.HTTPPath, HttpPort: p.HTTPPort, TcpPort: p.TCPPort,
		ExecCommand:         p.Exec,
		InitialDelaySeconds: p.InitialDelaySeconds, PeriodSeconds: p.PeriodSeconds,
		TimeoutSeconds: p.TimeoutSeconds, FailureThreshold: p.FailureThreshold,
		SuccessThreshold: p.SuccessThreshold,
	}
}

// fromRollout maps a rollout to the wire form (nil when unset).
func fromRollout(r servicesmgr.Rollout) *opsv1.RolloutSpec {
	if r.MaxSurge == "" && r.MaxUnavailable == "" {
		return nil
	}
	return &opsv1.RolloutSpec{MaxSurge: r.MaxSurge, MaxUnavailable: r.MaxUnavailable}
}

// presetFor renders a port's preset name from its (port, protocol).
func presetFor(port int32, protocol string) string {
	switch {
	case port == 80 && protocol == "tcp":
		return "tcp80"
	case port == 443 && protocol == "tcp":
		return "tcp443"
	case port == 443 && protocol == "udp":
		return "udp443"
	}
	return ""
}

// ---- misc helpers ----

// secureJoin joins a repo-relative path onto root, refusing anything that
// escapes root (absolute paths or `..` traversal).
func secureJoin(root, rel string) (string, error) {
	clean := path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	joined := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes %q", rel, root)
	}
	return joined, nil
}
