package servicesmgr

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// defaultServiceResources maps an explicit cpu/memory (request == limit) or the
// deployment's split defaults into k8s resource requirements.
func (c *Client) defaultServiceResources(cpu, mem string) corev1.ResourceRequirements {
	req := corev1.ResourceList{}
	lim := corev1.ResourceList{}
	if cpu != "" {
		// Explicit cpu/memory keep request == limit.
		q := resource.MustParse(cpu)
		req[corev1.ResourceCPU] = q
		lim[corev1.ResourceCPU] = q
	} else {
		req[corev1.ResourceCPU] = resource.MustParse(svcOr(c.res.cpuReq, "500m"))
		lim[corev1.ResourceCPU] = resource.MustParse(svcOr(c.res.cpuLim, "2"))
	}
	if mem != "" {
		q := resource.MustParse(mem)
		req[corev1.ResourceMemory] = q
		lim[corev1.ResourceMemory] = q
	} else {
		req[corev1.ResourceMemory] = resource.MustParse(svcOr(c.res.memReq, "1Gi"))
		lim[corev1.ResourceMemory] = resource.MustParse(svcOr(c.res.memLim, "4Gi"))
	}
	return corev1.ResourceRequirements{Requests: req, Limits: lim}
}

// svcOr returns v when non-empty, else def.
func svcOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// resources maps an explicit cpu/memory to k8s requirements (request == limit);
// empty when both are unset. Used for sidecar/init containers.
func resources(cpu, mem string) corev1.ResourceRequirements {
	if cpu == "" && mem == "" {
		return corev1.ResourceRequirements{}
	}
	rl := corev1.ResourceList{}
	if cpu != "" {
		rl[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		rl[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return corev1.ResourceRequirements{Requests: rl, Limits: rl}
}

// buildResources maps a split request/limit spec to k8s requirements.
func buildResources(r Resources) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{}
	req := corev1.ResourceList{}
	if r.CPURequest != "" {
		req[corev1.ResourceCPU] = resource.MustParse(r.CPURequest)
	}
	if r.MemoryRequest != "" {
		req[corev1.ResourceMemory] = resource.MustParse(r.MemoryRequest)
	}
	if len(req) > 0 {
		out.Requests = req
	}
	lim := corev1.ResourceList{}
	if r.CPULimit != "" {
		lim[corev1.ResourceCPU] = resource.MustParse(r.CPULimit)
	}
	if r.MemoryLimit != "" {
		lim[corev1.ResourceMemory] = resource.MustParse(r.MemoryLimit)
	}
	if len(lim) > 0 {
		out.Limits = lim
	}
	return out
}
