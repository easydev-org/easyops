package servicesmgr

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func toService(c *Client, d *appsv1.Deployment, ports []Port) Service {
	phase := "Pending"
	if d.Status.ReadyReplicas > 0 {
		phase = "Running"
	} else if d.Status.UnavailableReplicas > 0 {
		phase = "Progressing"
	}
	reps := int32(0)
	if d.Spec.Replicas != nil {
		reps = *d.Spec.Replicas
	}
	host := fmt.Sprintf("%s.%s.svc.cluster.local", d.Name, c.namespace)
	// The in-cluster URL points at the primary Service's first TCP port.
	urlPort := int32(80)
	for _, p := range ports {
		if p.Suffix == "" && p.Protocol == "tcp" {
			urlPort = p.Port
			break
		}
	}
	_, paused := d.Annotations[AnnoReplicasBeforePause]
	svc := Service{
		Name: d.Name, Image: d.Annotations[AnnoImage], Phase: phase,
		Ready: d.Status.ReadyReplicas > 0, Replicas: reps,
		ReadyReplicas: d.Status.ReadyReplicas,
		URL:           fmt.Sprintf("http://%s:%d", host, urlPort),
		Creator:       d.Annotations[AnnoCreator], Session: d.Annotations[AnnoSession],
		Namespace: d.Annotations[AnnoNamespace],
		CreatedAt: d.CreationTimestamp.UnixMilli(),
		Ports:     ports,
		Paused:    paused,
		Revision:  revisionOf(d.Annotations),
	}
	// Container detail (first container only — every managed service has one).
	if len(d.Spec.Template.Spec.Containers) > 0 {
		ctr := d.Spec.Template.Spec.Containers[0]
		// Prefer the live container image (rollback changes the pod template,
		// not the AnnoImage annotation) so the view reflects what will run.
		if ctr.Image != "" {
			svc.Image = ctr.Image
		}
		svc.Command = ctr.Command
		if len(ctr.Env) > 0 {
			svc.Env = map[string]string{}
			for _, e := range ctr.Env {
				svc.Env[e.Name] = e.Value
			}
		}
		if r, ok := ctr.Resources.Requests[corev1.ResourceCPU]; ok {
			svc.CPU = r.String()
		}
		if r, ok := ctr.Resources.Requests[corev1.ResourceMemory]; ok {
			svc.Memory = r.String()
		}
		svc.Resources = Resources{
			CPURequest:    quantityString(ctr.Resources.Requests, corev1.ResourceCPU),
			MemoryRequest: quantityString(ctr.Resources.Requests, corev1.ResourceMemory),
			CPULimit:      quantityString(ctr.Resources.Limits, corev1.ResourceCPU),
			MemoryLimit:   quantityString(ctr.Resources.Limits, corev1.ResourceMemory),
		}
		svc.ReadinessProbe = toProbe(ctr.ReadinessProbe)
		svc.LivenessProbe = toProbe(ctr.LivenessProbe)
		svc.StartupProbe = toProbe(ctr.StartupProbe)
		// Map pod volume names back to PVC / ConfigMap / Secret sources.
		volByPVC := map[string]string{}
		cmByVol := map[string]corev1.Volume{}
		for _, v := range d.Spec.Template.Spec.Volumes {
			cmByVol[v.Name] = v
			if v.PersistentVolumeClaim != nil {
				volByPVC[v.Name] = v.PersistentVolumeClaim.ClaimName
			}
		}
		for _, m := range ctr.VolumeMounts {
			if pvc, ok := volByPVC[m.Name]; ok {
				svc.Volumes = append(svc.Volumes, VolumeMount{
					PVC: pvc, MountPath: m.MountPath, ReadOnly: m.ReadOnly, SubPath: m.SubPath,
				})
				continue
			}
			if v, ok := cmByVol[m.Name]; ok {
				switch {
				case v.ConfigMap != nil:
					svc.ConfigMounts = append(svc.ConfigMounts, ConfigMount{ConfigMap: v.ConfigMap.Name, MountPath: m.MountPath})
				case v.Secret != nil:
					svc.ConfigMounts = append(svc.ConfigMounts, ConfigMount{Secret: v.Secret.SecretName, MountPath: m.MountPath})
				}
			}
		}
		// Extra containers (beyond the first) count as sidecars/init.
		svc.SidecarCount = int32(len(d.Spec.Template.Spec.Containers) - 1 + len(d.Spec.Template.Spec.InitContainers))
	}
	if ru := d.Spec.Strategy.RollingUpdate; ru != nil {
		if ru.MaxSurge != nil {
			svc.Rollout.MaxSurge = ru.MaxSurge.String()
		}
		if ru.MaxUnavailable != nil {
			svc.Rollout.MaxUnavailable = ru.MaxUnavailable.String()
		}
	}
	return svc
}

// quantityString renders a resource quantity ("" when absent).
// portsFor collects every port across the primary Service and its siblings.
func (c *Client) portsFor(ctx context.Context, name string) []Port {
	list, err := c.cs.CoreV1().Services(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1," + LabelName + "=" + name})
	if err != nil {
		return nil
	}
	out := []Port{}
	for i := range list.Items {
		out = append(out, portsFromService(&list.Items[i], name)...)
	}
	return out
}

// portsFromService maps one k8s Service to its ports; `name` is the logical
// service name (so the suffix is derived from a sibling `<name>-<suffix>`).
func portsFromService(svc *corev1.Service, name string) []Port {
	out := []Port{}
	suffix := ""
	if svc.Name != name {
		suffix = strings.TrimPrefix(svc.Name, name+"-")
	}
	for _, p := range svc.Spec.Ports {
		proto := "tcp"
		if p.Protocol == corev1.ProtocolUDP {
			proto = "udp"
		}
		out = append(out, Port{
			Suffix: suffix, Port: p.Port, Protocol: proto,
			TargetPort: p.TargetPort.IntVal,
		})
	}
	return out
}

func quantityString(rl corev1.ResourceList, name corev1.ResourceName) string {
	if q, ok := rl[name]; ok {
		return q.String()
	}
	return ""
}

// toProbe maps a k8s Probe back to our Probe spec.
func toProbe(p *corev1.Probe) Probe {
	if p == nil {
		return Probe{}
	}
	out := Probe{
		InitialDelaySeconds: p.InitialDelaySeconds,
		PeriodSeconds:       p.PeriodSeconds,
		TimeoutSeconds:      p.TimeoutSeconds,
		FailureThreshold:    p.FailureThreshold,
		SuccessThreshold:    p.SuccessThreshold,
	}
	switch {
	case p.HTTPGet != nil:
		out.HTTPPath = p.HTTPGet.Path
		out.HTTPPort = p.HTTPGet.Port.IntVal
	case p.TCPSocket != nil:
		out.TCPPort = p.TCPSocket.Port.IntVal
	case p.Exec != nil:
		out.Exec = p.Exec.Command
	}
	return out
}

// podDiagnostics returns the pod phase, total restarts and the first
// unhealthy container reason for a service (best-effort; zero values when no
// pod is observed). A crashlooping container reports its waiting reason so a
// failure is visible in the service list.
func (c *Client) podDiagnostics(ctx context.Context, name string) (phase string, restarts int32, message string) {
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + name})
	if err != nil || len(pods.Items) == 0 {
		return "", 0, ""
	}
	// Prefer the newest pod.
	pod := pods.Items[0]
	for i := range pods.Items {
		if pods.Items[i].CreationTimestamp.After(pod.CreationTimestamp.Time) {
			pod = pods.Items[i]
		}
	}
	phase = string(pod.Status.Phase)
	for _, cs := range pod.Status.ContainerStatuses {
		restarts += cs.RestartCount
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && message == "" {
			message = cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				message += ": " + cs.State.Waiting.Message
			}
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" && cs.State.Terminated.Reason != "Completed" && message == "" {
			message = cs.State.Terminated.Reason
		}
	}
	return phase, restarts, message
}

// ServiceName derives a k8s-safe service name from a session (org:repo:branch).
func ServiceName(session string) string {
	slug := strings.NewReplacer(":", "-", "/", "-", "_", "-", ".", "-").Replace(strings.ToLower(session))
	slug = strings.Trim(slug, "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	if slug == "" {
		return "app"
	}
	return slug
}

// diag is the per-service pod diagnostics summary.
type diag struct {
	phase    string
	restarts int32
	message  string
}

// portsByService maps each managed service name to its exposed ports, in ONE
// Services List (instead of a List per service). Reuses portsFromService so the
// suffix/port mapping stays identical to portsFor.
func (c *Client) portsByService(ctx context.Context) map[string][]Port {
	out := map[string][]Port{}
	list, err := c.cs.CoreV1().Services(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1"})
	if err != nil {
		return out
	}
	for i := range list.Items {
		svc := &list.Items[i]
		name := svc.Labels[LabelName]
		if name == "" {
			name = svc.Name
		}
		out[name] = append(out[name], portsFromService(svc, name)...)
	}
	return out
}

// podDiagnosticsByService maps each managed service name to its newest pod's
// diagnostics, in ONE Pods List (instead of a List per service). Pods carry the
// same `app=<service>` label as the Deployment selector.
func (c *Client) podDiagnosticsByService(ctx context.Context) map[string]diag {
	out := map[string]diag{}
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	newest := map[string]*corev1.Pod{}
	for i := range pods.Items {
		p := &pods.Items[i]
		name := p.Labels["app"]
		if name == "" {
			continue
		}
		if cur := newest[name]; cur == nil || p.CreationTimestamp.After(cur.CreationTimestamp.Time) {
			newest[name] = p
		}
	}
	for name, p := range newest {
		out[name] = diagFromPod(p)
	}
	return out
}

// diagFromPod summarizes a pod's phase/restarts/first-failure reason.
func diagFromPod(pod *corev1.Pod) diag {
	d := diag{phase: string(pod.Status.Phase)}
	for _, cs := range pod.Status.ContainerStatuses {
		d.restarts += cs.RestartCount
		if d.message == "" && cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			d.message = cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				d.message += ": " + cs.State.Waiting.Message
			}
		}
		if d.message == "" && cs.State.Terminated != nil && cs.State.Terminated.Reason != "" &&
			cs.State.Terminated.Reason != "Completed" {
			d.message = cs.State.Terminated.Reason
		}
	}
	return d
}
