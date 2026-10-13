package helmmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
)

// ---- release state (ConfigMap-backed) ----

func (c *Client) cmName(release string) string { return releaseCMName + release }

// Get returns a release, or ok=false when absent.
func (c *Client) Get(ctx context.Context, release string) (Release, bool, error) {
	cm, err := c.cs.CoreV1().ConfigMaps(c.namespace).Get(ctx, c.cmName(release), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Release{}, false, nil
	}
	if err != nil {
		return Release{}, false, err
	}
	var r Release
	if err := json.Unmarshal([]byte(cm.Data["release"]), &r); err != nil {
		return Release{}, false, err
	}
	return r, true, nil
}

func (c *Client) put(ctx context.Context, r Release) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: c.cmName(r.Name), Namespace: c.namespace,
			Labels: map[string]string{releaseLabel: r.Name, "app.kubernetes.io/managed-by": "agent-gateway"},
		},
		Data: map[string]string{"release": string(b)},
	}
	if _, err := c.cs.CoreV1().ConfigMaps(c.namespace).Create(ctx, cm, metav1.CreateOptions{}); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return err
	}
	cur, err := c.cs.CoreV1().ConfigMaps(c.namespace).Get(ctx, cm.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	cm.ResourceVersion = cur.ResourceVersion
	_, err = c.cs.CoreV1().ConfigMaps(c.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// List returns every release in the namespace (newest first).
func (c *Client) List(ctx context.Context) ([]Release, error) {
	cms, err := c.cs.CoreV1().ConfigMaps(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: releaseLabel})
	if err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(cms.Items))
	for i := range cms.Items {
		var r Release
		if err := json.Unmarshal([]byte(cms.Items[i].Data["release"]), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// WatchReleases returns a watch over the release-state ConfigMaps (each release
// keeps its state in one ConfigMap, so a change signals a release update). The
// caller re-lists on each signal, so only the event stream matters.
func (c *Client) WatchReleases(ctx context.Context) (watch.Interface, error) {
	return c.cs.CoreV1().ConfigMaps(c.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: releaseLabel,
	})
}

// Rollback re-applies a prior revision's manifest and records a new revision.
func (c *Client) Rollback(ctx context.Context, release string, revision int) (Release, error) {
	cur, ok, err := c.Get(ctx, release)
	if err != nil {
		return Release{}, err
	}
	if !ok {
		return Release{}, fmt.Errorf("release %q not found", release)
	}
	var target *Revision
	for i := range cur.History {
		if cur.History[i].Revision == revision {
			target = &cur.History[i]
			break
		}
	}
	if target == nil {
		return Release{}, fmt.Errorf("revision %d not found", revision)
	}
	if err := c.applyManifest(ctx, target.Manifest, release); err != nil {
		return Release{}, err
	}
	cur.Revision++
	cur.Status = "deployed"
	cur.UpdatedAt = time.Now().UnixMilli()
	cur.Ref = target.Ref
	cur.ChartPath = target.ChartPath
	cur.ChartVersion = target.ChartVersion
	cur.AppVersion = target.AppVersion
	cur.History = append(cur.History, Revision{
		Revision: cur.Revision, Ref: target.Ref, ChartPath: target.ChartPath,
		ChartVersion: target.ChartVersion, AppVersion: target.AppVersion, Values: target.Values,
		CreatedAt: cur.UpdatedAt, Objects: target.Objects, Manifest: target.Manifest,
	})
	if err := c.put(ctx, cur); err != nil {
		return Release{}, err
	}
	return cur, nil
}

// Uninstall deletes every object of the release (newest revision) and its
// release record.
func (c *Client) Uninstall(ctx context.Context, release string) (bool, error) {
	cur, ok, err := c.Get(ctx, release)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if len(cur.History) > 0 {
		last := cur.History[len(cur.History)-1]
		// Delete in reverse order (dependents first is best-effort).
		for i := len(last.Objects) - 1; i >= 0; i-- {
			_ = c.deleteObject(ctx, last.Objects[i])
		}
	}
	if err := c.cs.CoreV1().ConfigMaps(c.namespace).Delete(ctx, c.cmName(release), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

// applyManifest parses + applies every doc (server-side create-or-update).
func (c *Client) applyManifest(ctx context.Context, manifest, release string) error {
	docs := strings.Split(manifest, "\n---")
	for _, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var u unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			return err
		}
		if len(u.Object) == 0 || u.GetKind() == "" {
			continue
		}
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[releaseLabel] = release
		labels["app.kubernetes.io/managed-by"] = "agent-gateway"
		u.SetLabels(labels)
		u.SetNamespace(c.namespace)
		if err := c.applyObject(ctx, &u); err != nil {
			return fmt.Errorf("apply %s/%s: %w", u.GetKind(), u.GetName(), err)
		}
	}
	return nil
}

func (c *Client) applyObject(ctx context.Context, u *unstructured.Unstructured) error {
	mapping, err := c.mapper.RESTMapping(u.GroupVersionKind().GroupKind(), u.GroupVersionKind().Version)
	if err != nil {
		return err
	}
	ri := c.dyn.Resource(mapping.Resource).Namespace(c.namespace)
	cur, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err := ri.Create(ctx, u, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	u.SetResourceVersion(cur.GetResourceVersion())
	// Preserve an assigned ClusterIP for Services.
	if u.GetKind() == "Service" {
		if ip, found, _ := unstructured.NestedString(cur.Object, "spec", "clusterIP"); found && ip != "" {
			_ = unstructured.SetNestedField(u.Object, ip, "spec", "clusterIP")
		}
	}
	_, err = ri.Update(ctx, u, metav1.UpdateOptions{})
	return err
}

func (c *Client) deleteObject(ctx context.Context, o Object) error {
	gv, err := schema.ParseGroupVersion(o.APIVersion)
	if err != nil {
		return err
	}
	gvk := gv.WithKind(o.Kind)
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return err
	}
	return c.dyn.Resource(mapping.Resource).Namespace(c.namespace).Delete(ctx, o.Name, metav1.DeleteOptions{})
}
