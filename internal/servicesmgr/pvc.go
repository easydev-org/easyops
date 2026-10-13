// ---- persistent volume claims ----
package servicesmgr

import (
	"context"
	"fmt"
	"strings"

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
	return toPVC(p, c.mountedBy(ctx, name)), nil
}

// ListPVCs returns every managed claim.
func (c *Client) ListPVCs(ctx context.Context) ([]PVC, error) {
	list, err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelPVC + "=1"})
	if err != nil {
		return nil, err
	}
	// One Deployments List serves every claim (was: one List per claim).
	mounted := c.mountedIndex(ctx)
	out := make([]PVC, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, toPVC(&list.Items[i], mounted[list.Items[i].Name]))
	}
	return out, nil
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
	return PVC{
		Name: p.Name, Size: size, StorageClass: sc, Phase: string(p.Status.Phase),
		Creator: p.Annotations[AnnoPVCCreator], Namespace: p.Annotations[AnnoPVCNamespace],
		CreatedAt: p.CreationTimestamp.UnixMilli(),
		MountedBy: mountedBy,
	}
}
