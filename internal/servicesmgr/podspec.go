package servicesmgr

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// normalizePorts validates and groups the requested ports.
func normalizePorts(s Spec) ([]Port, map[string][]Port, []string, error) {
	ports := s.Ports
	if len(ports) == 0 {
		ports = []Port{{Port: 80, Protocol: "tcp", TargetPort: s.ContainerPort}}
	}
	groups := map[string][]Port{}
	order := []string{}
	for i := range ports {
		p := &ports[i]
		if p.Protocol == "" {
			p.Protocol = "tcp"
		}
		if p.Protocol != "tcp" && p.Protocol != "udp" {
			return nil, nil, nil, fmt.Errorf("port %d: protocol must be tcp|udp", i)
		}
		if p.Port != 80 && p.Port != 443 {
			return nil, nil, nil, fmt.Errorf("port %d: service port must be 80 or 443", i)
		}
		if p.TargetPort == 0 {
			p.TargetPort = s.ContainerPort
		}
		if _, dup := groups[p.Suffix]; !dup {
			order = append(order, p.Suffix)
		}
		groups[p.Suffix] = append(groups[p.Suffix], *p)
	}
	return ports, groups, order, nil
}

// buildPodSpec assembles the pod spec.
func (c *Client) buildPodSpec(s Spec, ports []Port) (corev1.PodSpec, error) {
	env := []corev1.EnvVar{}
	for k, v := range s.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	for k, v := range s.Runtime.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	for i, e := range s.EnvRefs {
		if e.Name == "" {
			return corev1.PodSpec{}, fmt.Errorf("env_refs[%d]: name is required", i)
		}
		switch {
		case e.ConfigMap != "" && e.ConfigKey != "":
			env = append(env, corev1.EnvVar{Name: e.Name, ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.ConfigMap}, Key: e.ConfigKey,
				},
			}})
		case e.Secret != "" && e.SecretKey != "":
			env = append(env, corev1.EnvVar{Name: e.Name, ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.Secret}, Key: e.SecretKey,
				},
			}})
		default:
			return corev1.PodSpec{}, fmt.Errorf("env_refs[%d]: set (config_map+config_key) or (secret+secret_key)", i)
		}
	}
	envFrom := []corev1.EnvFromSource{}
	for i, e := range s.EnvFrom {
		switch {
		case e.ConfigMap != "":
			envFrom = append(envFrom, corev1.EnvFromSource{ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: e.ConfigMap},
			}})
		case e.Secret != "":
			envFrom = append(envFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: e.Secret},
			}})
		default:
			return corev1.PodSpec{}, fmt.Errorf("env_from[%d]: set config_map or secret", i)
		}
	}
	rr := c.defaultServiceResources(s.CPU, s.Memory)
	if !s.Resources.Empty() {
		rr = buildResources(s.Resources)
	}
	if s.Runtime.Resources != nil {
		rr = *s.Runtime.Resources
	}
	for k, v := range s.Runtime.DeviceLimits {
		if rr.Limits == nil {
			rr.Limits = corev1.ResourceList{}
		}
		rr.Limits[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	containerPorts := []corev1.ContainerPort{}
	seen := map[int32]bool{}
	for _, p := range ports {
		if seen[p.TargetPort] {
			continue
		}
		seen[p.TargetPort] = true
		containerPorts = append(containerPorts, corev1.ContainerPort{
			Name: fmt.Sprintf("p%d", p.TargetPort), ContainerPort: p.TargetPort,
		})
	}
	container := corev1.Container{
		Name: "svc", Image: s.Image, Env: env, EnvFrom: envFrom, Command: s.Command,
		Ports: containerPorts, Resources: rr, SecurityContext: s.Runtime.SecurityContext,
	}
	if pr := buildProbe(s.ReadinessProbe); pr != nil {
		container.ReadinessProbe = pr
	}
	if pl := buildProbe(s.LivenessProbe); pl != nil {
		container.LivenessProbe = pl
	}
	if ps := buildProbe(s.StartupProbe); ps != nil {
		container.StartupProbe = ps
	}
	podSpec := corev1.PodSpec{Containers: []corev1.Container{container}}
	initContainers := []corev1.Container{}
	extraContainers := []corev1.Container{}
	for i, cm := range s.ConfigMounts {
		if cm.MountPath == "" {
			return corev1.PodSpec{}, fmt.Errorf("config_mounts[%d]: mount_path is required", i)
		}
		volName := fmt.Sprintf("cfg-%d", i)
		vol := corev1.Volume{Name: volName}
		switch {
		case cm.ConfigMap != "":
			src := &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cm.ConfigMap}}
			for _, it := range cm.Items {
				src.Items = append(src.Items, corev1.KeyToPath{Key: it.Key, Path: it.Path})
			}
			vol.ConfigMap = src
		case cm.Secret != "":
			src := &corev1.SecretVolumeSource{SecretName: cm.Secret}
			for _, it := range cm.Items {
				src.Items = append(src.Items, corev1.KeyToPath{Key: it.Key, Path: it.Path})
			}
			vol.Secret = src
		default:
			return corev1.PodSpec{}, fmt.Errorf("config_mounts[%d]: set config_map or secret", i)
		}
		podSpec.Volumes = append(podSpec.Volumes, vol)
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: volName, MountPath: cm.MountPath,
		})
	}
	for i, sc := range s.Sidecars {
		if sc.Name == "" || sc.Image == "" {
			return corev1.PodSpec{}, fmt.Errorf("sidecars[%d]: name and image are required", i)
		}
		cc := corev1.Container{Name: sc.Name, Image: sc.Image, Command: sc.Command, Resources: resources(sc.CPU, sc.Memory)}
		for k, v := range sc.Env {
			cc.Env = append(cc.Env, corev1.EnvVar{Name: k, Value: v})
		}
		if sc.Init {
			initContainers = append(initContainers, cc)
		} else {
			extraContainers = append(extraContainers, cc)
		}
	}
	if len(initContainers) > 0 {
		podSpec.InitContainers = initContainers
	}
	if len(extraContainers) > 0 {
		podSpec.Containers = append(podSpec.Containers, extraContainers...)
	}
	for i, t := range s.Tolerations {
		if t.Key == "" {
			return corev1.PodSpec{}, fmt.Errorf("tolerations[%d]: key is required", i)
		}
		podSpec.Tolerations = append(podSpec.Tolerations, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value, Effect: corev1.TaintEffect(t.Effect),
		})
	}
	if len(s.NodeSelector) > 0 {
		if podSpec.NodeSelector == nil {
			podSpec.NodeSelector = map[string]string{}
		}
		for k, v := range s.NodeSelector {
			podSpec.NodeSelector[k] = v
		}
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
		t := corev1.HostPathCharDev
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name:         "devtun",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/net/tun", Type: &t}},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: "devtun", MountPath: "/dev/net/tun",
		})
	}
	for i, vm := range s.Volumes {
		if vm.PVC == "" || vm.MountPath == "" {
			return corev1.PodSpec{}, fmt.Errorf("volumes[%d]: pvc and mount_path are required", i)
		}
		volName := fmt.Sprintf("pvc-%d", i)
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: vm.PVC, ReadOnly: vm.ReadOnly,
			}},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: volName, MountPath: vm.MountPath, ReadOnly: vm.ReadOnly, SubPath: vm.SubPath,
		})
	}
	return podSpec, nil
}
