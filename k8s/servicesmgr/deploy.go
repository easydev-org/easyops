package servicesmgr

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Deploy creates or updates the Deployment + its Service(s) for a spec
// (idempotent). Ports are grouped by suffix: "" -> the primary Service
// `<name>`; a non-empty suffix -> a sibling Service `<name>-<suffix>` sharing
// the same pod selector.
func (c *Client) Deploy(ctx context.Context, s Spec) (Service, error) {
	if s.Name == "" || s.Image == "" {
		return Service{}, fmt.Errorf("name and image required")
	}
	if s.ContainerPort == 0 {
		s.ContainerPort = 8080
	}
	ports := s.Ports
	if len(ports) == 0 {
		// Default: a single public port 80 -> the container port.
		ports = []Port{{Port: 80, Protocol: "tcp", TargetPort: s.ContainerPort}}
	}
	// Validate ports + group by suffix.
	groups := map[string][]Port{}
	order := []string{}
	for i := range ports {
		p := &ports[i]
		if p.Protocol == "" {
			p.Protocol = "tcp"
		}
		if p.Protocol != "tcp" && p.Protocol != "udp" {
			return Service{}, fmt.Errorf("port %d: protocol must be tcp|udp", i)
		}
		if p.Port != 80 && p.Port != 443 {
			return Service{}, fmt.Errorf("port %d: service port must be 80 or 443", i)
		}
		if p.TargetPort == 0 {
			p.TargetPort = s.ContainerPort
		}
		if _, dup := groups[p.Suffix]; !dup {
			order = append(order, p.Suffix)
		}
		groups[p.Suffix] = append(groups[p.Suffix], *p)
	}
	// Conflict precheck: every Service we would write must be either absent or
	// OUR managed object (same tenant). Never clobber a foreign Service.
	for _, suffix := range order {
		svcName := siblingName(s.Name, suffix)
		cur, err := c.cs.CoreV1().Services(c.namespace).Get(ctx, svcName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return Service{}, err
		}
		if cur.Labels[LabelManaged] != "1" || (s.Creator != "" && cur.Annotations[AnnoCreator] != s.Creator) {
			return Service{}, fmt.Errorf("service %q already exists and is not managed by this tenant", svcName)
		}
	}

	reps := s.Replicas
	if reps <= 0 {
		reps = 1
	}
	labels := map[string]string{LabelManaged: "1", LabelName: s.Name, "app": s.Name}
	annotations := map[string]string{
		AnnoImage: s.Image, AnnoCreator: s.Creator, AnnoSession: s.Session, AnnoNamespace: s.Namespace,
	}

	env := []corev1.EnvVar{}
	for k, v := range s.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	for k, v := range s.Runtime.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	// Env from ConfigMap/Secret keys, then whole-config envFrom.
	for i, e := range s.EnvRefs {
		if e.Name == "" {
			return Service{}, fmt.Errorf("env_refs[%d]: name is required", i)
		}
		switch {
		case e.ConfigMap != "" && e.ConfigKey != "":
			env = append(env, corev1.EnvVar{Name: e.Name, ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.ConfigMap},
					Key:                  e.ConfigKey,
				},
			}})
		case e.Secret != "" && e.SecretKey != "":
			env = append(env, corev1.EnvVar{Name: e.Name, ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.Secret},
					Key:                  e.SecretKey,
				},
			}})
		default:
			return Service{}, fmt.Errorf("env_refs[%d]: set (config_map+config_key) or (secret+secret_key)", i)
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
			return Service{}, fmt.Errorf("env_from[%d]: set config_map or secret", i)
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
	// Container ports: the distinct target ports across all exposed ports.
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
		Name:            "svc",
		Image:           s.Image,
		Env:             env,
		EnvFrom:         envFrom,
		Command:         s.Command,
		Ports:           containerPorts,
		Resources:       rr,
		SecurityContext: s.Runtime.SecurityContext,
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
	// ConfigMap/Secret volumes.
	initContainers := []corev1.Container{}
	extraContainers := []corev1.Container{}
	for i, cm := range s.ConfigMounts {
		if cm.MountPath == "" {
			return Service{}, fmt.Errorf("config_mounts[%d]: mount_path is required", i)
		}
		volName := fmt.Sprintf("cfg-%d", i)
		vol := corev1.Volume{Name: volName}
		switch {
		case cm.ConfigMap != "":
			src := &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cm.ConfigMap},
			}
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
			return Service{}, fmt.Errorf("config_mounts[%d]: set config_map or secret", i)
		}
		podSpec.Volumes = append(podSpec.Volumes, vol)
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: volName, MountPath: cm.MountPath,
		})
	}
	// Extra containers (init or sidecar).
	for i, sc := range s.Sidecars {
		if sc.Name == "" || sc.Image == "" {
			return Service{}, fmt.Errorf("sidecars[%d]: name and image are required", i)
		}
		c := corev1.Container{
			Name:      sc.Name,
			Image:     sc.Image,
			Command:   sc.Command,
			Resources: resources(sc.CPU, sc.Memory),
		}
		for k, v := range sc.Env {
			c.Env = append(c.Env, corev1.EnvVar{Name: k, Value: v})
		}
		if sc.Init {
			initContainers = append(initContainers, c)
		} else {
			extraContainers = append(extraContainers, c)
		}
	}
	if len(initContainers) > 0 {
		podSpec.InitContainers = initContainers
	}
	if len(extraContainers) > 0 {
		podSpec.Containers = append(podSpec.Containers, extraContainers...)
	}
	// Tolerations (node selector merges with the runtime profile's below).
	if len(s.Tolerations) > 0 {
		for i, t := range s.Tolerations {
			if t.Key == "" {
				return Service{}, fmt.Errorf("tolerations[%d]: key is required", i)
			}
			podSpec.Tolerations = append(podSpec.Tolerations, corev1.Toleration{
				Key: t.Key, Operator: corev1.TolerationOperator(t.Operator),
				Value: t.Value, Effect: corev1.TaintEffect(t.Effect),
			})
		}
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
			Name: "devtun",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: "/dev/net/tun", Type: &t,
			}},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: "devtun", MountPath: "/dev/net/tun",
		})
	}
	// Named PVC mounts. A claim is referenced by name in the managed namespace.
	for i, vm := range s.Volumes {
		if vm.PVC == "" || vm.MountPath == "" {
			return Service{}, fmt.Errorf("volumes[%d]: pvc and mount_path are required", i)
		}
		volName := fmt.Sprintf("pvc-%d", i)
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: vm.PVC,
					ReadOnly:  vm.ReadOnly,
				},
			},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: volName, MountPath: vm.MountPath, ReadOnly: vm.ReadOnly, SubPath: vm.SubPath,
		})
	}
	depSpec := appsv1.DeploymentSpec{
		Replicas: &reps,
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": s.Name}},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
			Spec:       podSpec,
		},
	}
	if ru := buildRollout(s.Rollout); ru != nil {
		depSpec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: ru,
		}
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: c.namespace, Labels: labels, Annotations: annotations},
		Spec:       depSpec,
	}
	if err := c.applyDeployment(ctx, dep); err != nil {
		return Service{}, err
	}

	// One Service per suffix, each selecting the same pod.
	for _, suffix := range order {
		svcName := siblingName(s.Name, suffix)
		svcLabels := map[string]string{LabelManaged: "1", LabelName: s.Name, "app": s.Name}
		svcAnnotations := map[string]string{
			AnnoImage: s.Image, AnnoCreator: s.Creator, AnnoSession: s.Session, AnnoNamespace: s.Namespace,
			AnnoPrimary: s.Name,
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: c.namespace, Labels: svcLabels, Annotations: svcAnnotations},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": s.Name},
				Ports:    corePorts(groups[suffix]),
			},
		}
		if err := c.applyService(ctx, svc); err != nil {
			return Service{}, err
		}
	}
	return c.Get(ctx, s.Name)
}

// siblingName is the k8s Service name for a port group: the primary name for
// the "" suffix, else `<name>-<suffix>`.
func siblingName(name, suffix string) string {
	if suffix == "" {
		return name
	}
	return name + "-" + suffix
}

// corePorts converts our Port list to k8s ServicePorts (names are unique per
// Service, so tcp443/udp443 can coexist).
func corePorts(ports []Port) []corev1.ServicePort {
	out := make([]corev1.ServicePort, 0, len(ports))
	for _, p := range ports {
		proto := corev1.ProtocolTCP
		if p.Protocol == "udp" {
			proto = corev1.ProtocolUDP
		}
		out = append(out, corev1.ServicePort{
			Name:       fmt.Sprintf("%s-%d", p.Protocol, p.Port),
			Port:       p.Port,
			Protocol:   proto,
			TargetPort: intstr.FromInt32(p.TargetPort),
		})
	}
	return out
}

// AnnoPrimary marks a sibling Service with the primary service it belongs to.
const AnnoPrimary = "workspace/service-primary"

func (c *Client) applyDeployment(ctx context.Context, dep *appsv1.Deployment) error {
	_, err := c.cs.AppsV1().Deployments(c.namespace).Create(ctx, dep, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		cur, gerr := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, dep.Name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		dep.ResourceVersion = cur.ResourceVersion
		_, err = c.cs.AppsV1().Deployments(c.namespace).Update(ctx, dep, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("apply deployment: %w", err)
	}
	return nil
}

func (c *Client) applyService(ctx context.Context, svc *corev1.Service) error {
	_, err := c.cs.CoreV1().Services(c.namespace).Create(ctx, svc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		cur, gerr := c.cs.CoreV1().Services(c.namespace).Get(ctx, svc.Name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		svc.ResourceVersion = cur.ResourceVersion
		svc.Spec.ClusterIP = cur.Spec.ClusterIP
		_, err = c.cs.CoreV1().Services(c.namespace).Update(ctx, svc, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("apply service: %w", err)
	}
	return nil
}

// buildProbe maps a Probe spec to a k8s Probe (nil when no action is set).
func buildProbe(p Probe) *corev1.Probe {
	if p.Empty() {
		return nil
	}
	handler := corev1.ProbeHandler{}
	switch {
	case p.HTTPPort > 0:
		path := p.HTTPPath
		if path == "" {
			path = "/"
		}
		handler.HTTPGet = &corev1.HTTPGetAction{
			Path: path, Port: intstr.FromInt32(p.HTTPPort),
		}
	case p.TCPPort > 0:
		handler.TCPSocket = &corev1.TCPSocketAction{Port: intstr.FromInt32(p.TCPPort)}
	case len(p.Exec) > 0:
		handler.Exec = &corev1.ExecAction{Command: p.Exec}
	}
	probe := &corev1.Probe{ProbeHandler: handler}
	if p.InitialDelaySeconds > 0 {
		probe.InitialDelaySeconds = p.InitialDelaySeconds
	}
	if p.PeriodSeconds > 0 {
		probe.PeriodSeconds = p.PeriodSeconds
	}
	if p.TimeoutSeconds > 0 {
		probe.TimeoutSeconds = p.TimeoutSeconds
	}
	if p.FailureThreshold > 0 {
		probe.FailureThreshold = p.FailureThreshold
	}
	if p.SuccessThreshold > 0 {
		probe.SuccessThreshold = p.SuccessThreshold
	}
	return probe
}

// buildRollout maps a Rollout spec to k8s int-or-percent values (nil when
// neither is set).
func buildRollout(r Rollout) *appsv1.RollingUpdateDeployment {
	if r.MaxSurge == "" && r.MaxUnavailable == "" {
		return nil
	}
	out := &appsv1.RollingUpdateDeployment{}
	if r.MaxSurge != "" {
		v := intstr.Parse(r.MaxSurge)
		out.MaxSurge = &v
	}
	if r.MaxUnavailable != "" {
		v := intstr.Parse(r.MaxUnavailable)
		out.MaxUnavailable = &v
	}
	return out
}
