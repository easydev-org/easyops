// ---- persistent volume claims ----
package servicesmgr

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PVC is a live PersistentVolumeClaim view.
type PVC struct {
	Name         string
	Size         string
	StorageClass string
	Phase        string
	Creator      string
	// Namespace is the tenant-internal namespace (org segment of the session).
	Namespace string
	CreatedAt int64
	// MountedBy lists the managed Deployments in the namespace that mount this
	// claim (best effort).
	MountedBy []string
	// BoundVolume is the PersistentVolume the claim is bound to ("" while it is
	// still pending).
	BoundVolume string
	// Message explains a non-Bound claim. A PVC has only three phases
	// (Pending/Bound/Lost) and `Pending` covers BOTH "the provisioner is still
	// working" and "provisioning FAILED", so the phase alone cannot tell them
	// apart. This carries the failing event's reason + message (e.g.
	// ProvisioningFailed) or the waiting event's text (e.g. "waiting for first
	// consumer to be created before binding").
	Message string
	// AgeSeconds is how long the claim has existed, so a caller can judge a
	// Pending claim: seconds = normal, many minutes = stuck.
	AgeSeconds int64
	// Capacity is the size actually ALLOCATED (Status.Capacity), which differs
	// from Size (the request) when a provisioner rounds up.
	Capacity string
}

// CreatePVC creates a named claim in the managed namespace with the given
// storage class (idempotent: an existing claim of the same name is returned
// unchanged, and its creator must match).
func (c *Client) CreatePVC(ctx context.Context, name, size, storageClass, creator, namespace string) (PVC, error) {
	if name == "" {
		return PVC{}, fmt.Errorf("name required")
	}
	if size == "" {
		size = "1Gi"
	}
	q, err := resource.ParseQuantity(size)
	if err != nil {
		return PVC{}, fmt.Errorf("invalid size %q: %w", size, err)
	}
	if existing, gerr := c.GetPVC(ctx, name); gerr == nil {
		if existing.Creator != "" && creator != "" && existing.Creator != creator {
			return PVC{}, fmt.Errorf("pvc %q belongs to another tenant", name)
		}
		return existing, nil
	} else if !apierrors.IsNotFound(gerr) {
		return PVC{}, gerr
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   c.namespace,
			Labels:      map[string]string{LabelPVC: "1", LabelPVCName: name},
			Annotations: map[string]string{AnnoPVCCreator: creator, AnnoPVCNamespace: namespace},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: q}},
		},
	}
	if storageClass != "" {
		sc := storageClass
		pvc.Spec.StorageClassName = &sc
	}
	if _, err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return PVC{}, fmt.Errorf("create pvc: %w", err)
	}
	return c.GetPVC(ctx, name)
}

// GetPVC returns one claim by name.
func (c *Client) GetPVC(ctx context.Context, name string) (PVC, error) {
	p, err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return PVC{}, err
	}
	v := toPVC(p, c.mountedBy(ctx, name))
	// Only a claim that is not Bound needs an explanation; skip the Events
	// query for the healthy case (it is the common case and costs a round-trip).
	if v.Phase != string(corev1.ClaimBound) {
		v.Message = c.pvcMessage(ctx, name)
	}
	return v, nil
}

// ListPVCs returns claims in the managed namespace.
//
// By default only claims THIS manager created are returned (they carry the
// workspace/pvc=1 label). That is the tenant-safe view: the platform's own
// claims (postgres-data, garage-data, buildkitd-data, ...) are none of a
// tenant's business, and a tenant has no way to tell them apart otherwise.
//
// includeAll drops the label filter and returns EVERY claim in the namespace.
// A claim created outside this manager has no creator annotation, so it is
// reported with an empty Creator — there is nothing to attribute it to.
func (c *Client) ListPVCs(ctx context.Context, includeAll bool) ([]PVC, error) {
	opts := metav1.ListOptions{LabelSelector: LabelPVC + "=1"}
	if includeAll {
		opts = metav1.ListOptions{}
	}
	list, err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	// One Deployments List serves every claim (was: one List per claim).
	mounted := c.mountedIndex(ctx)
	out := make([]PVC, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, toPVC(&list.Items[i], mounted[list.Items[i].Name]))
	}
	// One Events List serves every non-Bound claim (same batching idea as the
	// mounted index above).
	msgs := c.pvcMessages(ctx, list.Items)
	for i := range out {
		out[i].Message = msgs[out[i].Name]
	}
	return out, nil
}

// pvcMessage returns the most informative event text for one claim.
func (c *Client) pvcMessage(ctx context.Context, name string) string {
	ev, err := c.cs.CoreV1().Events(c.namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=PersistentVolumeClaim,involvedObject.name=" + name,
	})
	if err != nil {
		return ""
	}
	return pickPVCEvent(ev.Items)
}

// pvcMessages resolves the message for every NON-Bound claim in one Events List.
func (c *Client) pvcMessages(ctx context.Context, items []corev1.PersistentVolumeClaim) map[string]string {
	out := map[string]string{}
	var pending []string
	for i := range items {
		if string(items[i].Status.Phase) != string(corev1.ClaimBound) {
			pending = append(pending, items[i].Name)
		}
	}
	if len(pending) == 0 {
		return out
	}
	ev, err := c.cs.CoreV1().Events(c.namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=PersistentVolumeClaim",
	})
	if err != nil {
		return out
	}
	byName := map[string][]corev1.Event{}
	for _, e := range ev.Items {
		byName[e.InvolvedObject.Name] = append(byName[e.InvolvedObject.Name], e)
	}
	for _, n := range pending {
		if m := pickPVCEvent(byName[n]); m != "" {
			out[n] = m
		}
	}
	return out
}

// pickPVCEvent prefers a WARNING event (a real failure) over a Normal one, and
// the newest of each kind. A PVC emits both "Provisioning"/"WaitingForFirstConsumer"
// (Normal) and "ProvisioningFailed" (Warning); only the warning explains a
// claim that will never bind.
func pickPVCEvent(events []corev1.Event) string {
	var warn, normal *corev1.Event
	for i := range events {
		e := &events[i]
		if e.Type == corev1.EventTypeWarning {
			if warn == nil || e.LastTimestamp.After(warn.LastTimestamp.Time) {
				warn = e
			}
			continue
		}
		if normal == nil || e.LastTimestamp.After(normal.LastTimestamp.Time) {
			normal = e
		}
	}
	pick := warn
	if pick == nil {
		pick = normal
	}
	if pick == nil {
		return ""
	}
	if pick.Message != "" {
		if pick.Reason != "" {
			return pick.Reason + ": " + pick.Message
		}
		return pick.Message
	}
	return pick.Reason
}

// DeletePVC removes a claim. It REFUSES when any managed Deployment still mounts
// it, so a running service never loses its volume under it.
func (c *Client) DeletePVC(ctx context.Context, name string) (bool, error) {
	mounted := c.mountedBy(ctx, name)
	if len(mounted) > 0 {
		return false, fmt.Errorf("pvc %q is mounted by: %s", name, strings.Join(mounted, ", "))
	}
	err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// mountedBy returns the names of managed Deployments that mount the claim.
func (c *Client) mountedBy(ctx context.Context, name string) []string {
	return c.mountedIndex(ctx)[name]
}

// mountedIndex maps each claim name to the managed Deployments that mount it,
// from ONE Deployments List (so a PVC list is O(1) k8s calls, not O(N)).
func (c *Client) mountedIndex(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	deps, err := c.cs.AppsV1().Deployments(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1"})
	if err != nil {
		return out
	}
	for i := range deps.Items {
		for _, v := range deps.Items[i].Spec.Template.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != "" {
				out[v.PersistentVolumeClaim.ClaimName] = append(out[v.PersistentVolumeClaim.ClaimName], deps.Items[i].Name)
			}
		}
	}
	return out
}

func toPVC(p *corev1.PersistentVolumeClaim, mountedBy []string) PVC {
	size := ""
	if q, ok := p.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		size = q.String()
	}
	sc := ""
	if p.Spec.StorageClassName != nil {
		sc = *p.Spec.StorageClassName
	}
	// Capacity is what was ACTUALLY allocated; a provisioner may round the
	// request up, so it is worth reporting separately from the request.
	capacity := ""
	if q, ok := p.Status.Capacity[corev1.ResourceStorage]; ok {
		capacity = q.String()
	}
	v := PVC{
		Name: p.Name, Size: size, StorageClass: sc, Phase: string(p.Status.Phase),
		Creator: p.Annotations[AnnoPVCCreator], Namespace: p.Annotations[AnnoPVCNamespace],
		CreatedAt:   p.CreationTimestamp.UnixMilli(),
		MountedBy:   mountedBy,
		BoundVolume: p.Spec.VolumeName,
		Capacity:    capacity,
	}
	if !p.CreationTimestamp.IsZero() {
		v.AgeSeconds = int64(time.Since(p.CreationTimestamp.Time).Seconds())
	}
	return v
}
