package helmmgr

import (
	"fmt"
	"sort"
	"strings"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Allowed kinds a rendered manifest may contain. Anything else is refused, as
// are cluster-scoped resources, RBAC, CRDs and privileged pod features.
var allowedKinds = map[string]bool{
	"ConfigMap":               true,
	"Secret":                  true,
	"Service":                 true,
	"ServiceAccount":          true,
	"PersistentVolumeClaim":   true,
	"Deployment":              true,
	"StatefulSet":             true,
	"DaemonSet":               true,
	"Job":                     true,
	"CronJob":                 true,
	"Ingress":                 true,
	"NetworkPolicy":           true,
	"HorizontalPodAutoscaler": true,
	"PodDisruptionBudget":     true,
}

// Template renders the chart at chartDir (an extracted repo root + ChartPath)
// into a filtered, namespace-pinned manifest. It returns the rendered docs, the
// applied objects, and the chart's `version`/`appVersion` (dry-run = no apply).
func (c *Client) Template(chartDir string, o RenderOptions) (string, []Object, string, string, error) {
	ch, err := loader.LoadDir(chartDir)
	if err != nil {
		return "", nil, "", "", fmt.Errorf("load chart: %w", err)
	}
	vals := map[string]any{}
	if strings.TrimSpace(o.ValuesYAML) != "" {
		if err := yaml.Unmarshal([]byte(o.ValuesYAML), &vals); err != nil {
			return "", nil, "", "", fmt.Errorf("parse values: %w", err)
		}
	}
	relOpts := chartutil.ReleaseOptions{Name: o.Release, Namespace: c.namespace, Revision: 1, IsInstall: true}
	caps := chartutil.DefaultCapabilities
	renderVals, err := chartutil.ToRenderValues(ch, vals, relOpts, caps)
	if err != nil {
		return "", nil, "", "", fmt.Errorf("render values: %w", err)
	}
	rendered, err := engine.Render(ch, renderVals)
	if err != nil {
		return "", nil, "", "", fmt.Errorf("render: %w", err)
	}
	// Collect the rendered templates in a deterministic order.
	names := make([]string, 0, len(rendered))
	for n := range rendered {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	first := true
	for _, n := range names {
		if strings.HasSuffix(n, "_helpers.tpl") || strings.HasSuffix(n, "NOTES.txt") {
			continue
		}
		content := strings.TrimSpace(rendered[n])
		if content == "" {
			continue
		}
		if !first {
			b.WriteString("\n---\n")
		}
		b.WriteString(content)
		first = false
	}
	manifest := b.String()
	objects, err := c.validate(manifest)
	if err != nil {
		return "", nil, "", "", err
	}
	var chartVersion, appVersion string
	if ch.Metadata != nil {
		chartVersion = ch.Metadata.Version
		appVersion = ch.Metadata.AppVersion
	}
	return manifest, objects, chartVersion, appVersion, nil
}

// workloadPodSpecs are the unstructured paths to a workload's pod template
// "spec" (where labels + selector live).
var workloadPodSpecs = map[string][][]string{
	"Deployment":  {{"spec", "template", "spec"}},
	"StatefulSet": {{"spec", "template", "spec"}},
	"DaemonSet":   {{"spec", "template", "spec"}},
	"Job":         {{"spec", "template", "spec"}},
	"CronJob":     {{"spec", "jobTemplate", "spec", "template", "spec"}},
}

// workloadPodLabels are the matching pod-template LABELS paths (so a Service
// selector can match the pods).
var workloadPodLabels = map[string][][]string{
	"Deployment":  {{"spec", "template", "metadata", "labels"}},
	"StatefulSet": {{"spec", "template", "metadata", "labels"}},
	"DaemonSet":   {{"spec", "template", "metadata", "labels"}},
	"Job":         {{"spec", "template", "metadata", "labels"}},
	"CronJob":     {{"spec", "jobTemplate", "spec", "template", "metadata", "labels"}},
}

// validate parses every non-empty YAML doc, enforces the kind whitelist and
// namespace, strips dangerous fields, and returns the object identities.
func (c *Client) validate(manifest string) ([]Object, error) {
	var out []Object
	docs := strings.Split(manifest, "\n---")
	for _, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var u unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			return nil, fmt.Errorf("parse rendered doc: %w", err)
		}
		if len(u.Object) == 0 {
			continue
		}
		kind := u.GetKind()
		if kind == "" {
			continue
		}
		if !allowedKinds[kind] {
			return nil, fmt.Errorf("kind %q is not allowed", kind)
		}
		// No cluster-scoped objects: a namespaced kind must set/accept a namespace.
		ns := u.GetNamespace()
		if ns != "" && ns != c.namespace {
			return nil, fmt.Errorf("%s/%s: namespace %q is not allowed (only %q)", kind, u.GetName(), ns, c.namespace)
		}
		if isClusterScoped(c.mapper, u.GroupVersionKind()) {
			return nil, fmt.Errorf("%s is cluster-scoped and not allowed", kind)
		}
		if err := forbidDangerous(&u); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", kind, u.GetName(), err)
		}
		out = append(out, Object{APIVersion: u.GetAPIVersion(), Kind: kind, Name: u.GetName()})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("chart rendered no objects")
	}
	return out, nil
}

// forbidDangerous rejects privileged/host-access pod features anywhere in the doc.
func forbidDangerous(u *unstructured.Unstructured) error {
	bad := []struct {
		path []string
		want any
		why  string
	}{
		{[]string{"spec", "template", "spec", "hostNetwork"}, true, "hostNetwork"},
		{[]string{"spec", "template", "spec", "hostPID"}, true, "hostPID"},
		{[]string{"spec", "template", "spec", "hostIPC"}, true, "hostIPC"},
	}
	for _, b := range bad {
		if v, found, _ := unstructured.NestedFieldNoCopy(u.Object, b.path...); found && v == b.want {
			return fmt.Errorf("%s is not allowed", b.why)
		}
	}
	// Reject hostPath volumes and privileged containers in the pod template.
	for _, base := range [][]string{{"spec", "template", "spec"}, {"spec", "jobTemplate", "spec", "template", "spec"}} {
		vols, _, _ := unstructured.NestedSlice(u.Object, append(append([]string{}, base...), "volumes")...)
		for _, v := range vols {
			if m, ok := v.(map[string]any); ok {
				if _, has := m["hostPath"]; has {
					return fmt.Errorf("hostPath volumes are not allowed")
				}
			}
		}
		containers, _, _ := unstructured.NestedSlice(u.Object, append(append([]string{}, base...), "containers")...)
		for _, cn := range containers {
			if m, ok := cn.(map[string]any); ok {
				if sc, ok := m["securityContext"].(map[string]any); ok {
					if p, _ := sc["privileged"].(bool); p {
						return fmt.Errorf("privileged containers are not allowed")
					}
				}
			}
		}
	}
	return nil
}

func isClusterScoped(mapper meta.RESTMapper, gvk schema.GroupVersionKind) bool {
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return false
	}
	return mapping.Scope.Name() == meta.RESTScopeNameRoot
}
