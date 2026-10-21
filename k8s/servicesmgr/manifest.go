package servicesmgr

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Manifest returns the service's Deployment + every bound Service as a
// multi-document YAML (`---`-separated, Deployment first). Server-managed
// metadata (status, uid, resourceVersion, creationTimestamp, managedFields) is
// stripped so the result is a clean, re-appliable spec.
func (c *Client) Manifest(ctx context.Context, name string) (string, error) {
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	docs := []any{cleanDeployment(d)}
	svcs, lerr := c.cs.CoreV1().Services(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1," + LabelName + "=" + name})
	if lerr == nil {
		// Primary first, then siblings by name.
		sort.SliceStable(svcs.Items, func(i, j int) bool { return svcs.Items[i].Name < svcs.Items[j].Name })
		for i := range svcs.Items {
			docs = append(docs, cleanService(&svcs.Items[i]))
		}
	}
	var b strings.Builder
	for i, doc := range docs {
		if i > 0 {
			b.WriteString("---\n")
		}
		y, merr := yaml.Marshal(doc)
		if merr != nil {
			return "", merr
		}
		b.Write(y)
	}
	return b.String(), nil
}

// cleanDeployment deep-copies a Deployment with server-managed fields cleared,
// so it round-trips through YAML/apply cleanly.
func cleanDeployment(d *appsv1.Deployment) *appsv1.Deployment {
	out := d.DeepCopy()
	out.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
	out.Status = appsv1.DeploymentStatus{}
	out.ResourceVersion = ""
	out.UID = ""
	out.CreationTimestamp = metav1.Time{}
	out.ManagedFields = nil
	out.Generation = 0
	out.SelfLink = ""
	// Drop server-managed annotations (rollout revision, restart marker) so the
	// YAML round-trips cleanly.
	delete(out.Annotations, "deployment.kubernetes.io/revision")
	delete(out.Annotations, "kubectl.kubernetes.io/restartedAt")
	if out.Spec.Template.ObjectMeta.Annotations != nil {
		delete(out.Spec.Template.ObjectMeta.Annotations, "kubectl.kubernetes.io/restartedAt")
	}
	return out
}

func cleanService(s *corev1.Service) *corev1.Service {
	out := s.DeepCopy()
	out.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}
	out.ResourceVersion = ""
	out.UID = ""
	out.CreationTimestamp = metav1.Time{}
	out.ManagedFields = nil
	out.Generation = 0
	out.SelfLink = ""
	// Server-assigned networking fields.
	out.Spec.ClusterIP = ""
	out.Spec.ClusterIPs = nil
	out.Spec.IPFamilies = nil
	out.Spec.IPFamilyPolicy = nil
	out.Spec.HealthCheckNodePort = 0
	for i := range out.Spec.Ports {
		out.Spec.Ports[i].NodePort = 0
	}
	out.Status = corev1.ServiceStatus{}
	return out
}

// ApplyManifest parses a multi-document YAML (a Deployment + its Services) and
// applies it to the EXISTING service `name`. The Deployment's name, namespace,
// selector and managed labels are pinned so the manifest cannot retarget
// another service. Service objects are accepted only when their
// `workspace/service-name` label equals `name`. `dryRun` validates + returns
// the normalized YAML without writing.
func (c *Client) ApplyManifest(ctx context.Context, name, manifest string, dryRun bool) (Service, string, error) {
	cur, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, "", err
	}
	var dep *appsv1.Deployment
	var svcs []*corev1.Service
	for _, doc := range splitYAMLDocs(manifest) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			return Service{}, "", fmt.Errorf("parse yaml: %w", err)
		}
		switch strings.ToLower(probe.Kind) {
		case "deployment":
			var d appsv1.Deployment
			if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
				return Service{}, "", fmt.Errorf("parse deployment: %w", err)
			}
			dep = &d
		case "service":
			var s corev1.Service
			if err := yaml.Unmarshal([]byte(doc), &s); err != nil {
				return Service{}, "", fmt.Errorf("parse service: %w", err)
			}
			svcs = append(svcs, &s)
		default:
			return Service{}, "", fmt.Errorf("unsupported kind %q (only Deployment + Service)", probe.Kind)
		}
	}
	if dep == nil {
		return Service{}, "", fmt.Errorf("manifest has no Deployment")
	}
	// Pin identity: name/namespace/selector/labels must match the existing
	// managed service so a manifest cannot retarget another Deployment.
	if dep.Name != "" && dep.Name != name {
		return Service{}, "", fmt.Errorf("deployment name %q does not match %q", dep.Name, name)
	}
	dep.Name = name
	dep.Namespace = c.namespace
	dep.Labels = map[string]string{LabelManaged: "1", LabelName: name, "app": name}
	dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}
	dep.Spec.Template.ObjectMeta.Labels = map[string]string{LabelManaged: "1", LabelName: name, "app": name}
	// Preserve managed annotations (creator/session/stage/expiry) from the
	// current object; keep any user annotations.
	dep.Annotations = mergeAnnotations(cur.Annotations, dep.Annotations)
	for i := range svcs {
		s := svcs[i]
		if s.Labels[LabelName] != "" && s.Labels[LabelName] != name {
			return Service{}, "", fmt.Errorf("service %q belongs to another service", s.Name)
		}
		s.Namespace = c.namespace
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels[LabelManaged] = "1"
		s.Labels[LabelName] = name
		s.Labels["app"] = name
		s.Spec.Selector = map[string]string{"app": name}
		s.Spec.ClusterIP = ""
		s.Spec.ClusterIPs = nil
	}
	// Build the normalized YAML for the response.
	normalized, err := manifestYAML(dep, svcs)
	if err != nil {
		return Service{}, "", err
	}
	if dryRun {
		return Service{}, normalized, nil
	}
	// Apply the Deployment, then each Service (create-or-update).
	if err := c.applyDeployment(ctx, dep); err != nil {
		return Service{}, "", err
	}
	for i := range svcs {
		if err := c.applyService(ctx, svcs[i]); err != nil {
			return Service{}, "", err
		}
	}
	out, err := c.Get(ctx, name)
	if err != nil {
		return Service{}, "", err
	}
	return out, normalized, nil
}

// mergeAnnotations keeps `base` (server-managed) values unless `over` overrides
// them, and never lets an apply drop a managed annotation.
func mergeAnnotations(base, over map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range over {
		out[k] = v
	}
	for _, k := range []string{AnnoImage, AnnoCreator, AnnoSession} {
		if v, ok := base[k]; ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func manifestYAML(dep *appsv1.Deployment, svcs []*corev1.Service) (string, error) {
	var b strings.Builder
	y, err := yaml.Marshal(dep)
	if err != nil {
		return "", err
	}
	b.Write(y)
	for _, s := range svcs {
		b.WriteString("---\n")
		y, err := yaml.Marshal(s)
		if err != nil {
			return "", err
		}
		b.Write(y)
	}
	return b.String(), nil
}

// splitYAMLDocs splits a multi-document YAML on lines that are exactly `---`.
func splitYAMLDocs(manifest string) []string {
	var docs []string
	var cur []string
	for _, line := range strings.Split(manifest, "\n") {
		if strings.TrimSpace(line) == "---" {
			docs = append(docs, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	docs = append(docs, strings.Join(cur, "\n"))
	return docs
}
