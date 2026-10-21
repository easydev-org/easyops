package servicesmgr

import (
	"context"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Get returns one service by name.
func (c *Client) Get(ctx context.Context, name string) (Service, error) {
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, err
	}
	svc := toService(c, d, c.portsFor(ctx, name))
	svc.PodPhase, svc.Restarts, svc.Message = c.podDiagnostics(ctx, name)
	return svc, nil
}

// Exists reports whether a managed service exists.
func (c *Client) Exists(ctx context.Context, name string) (bool, error) {
	_, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// List returns every managed service.
func (c *Client) List(ctx context.Context) ([]Service, error) {
	list, err := c.cs.AppsV1().Deployments(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1"})
	if err != nil {
		return nil, err
	}
	// Batch the per-service lookups (ports + pod diagnostics) into THREE List
	// calls instead of 2*N point calls: the old N+1 pattern (a Services List and
	// a Pods List per Deployment) dominated the latency of every list view.
	portsBy := c.portsByService(ctx)
	diagBy := c.podDiagnosticsByService(ctx)
	out := make([]Service, 0, len(list.Items))
	for i := range list.Items {
		d := &list.Items[i]
		svc := toService(c, d, portsBy[d.Name])
		svc.PodPhase, svc.Restarts, svc.Message = diagBy[d.Name].phase, diagBy[d.Name].restarts, diagBy[d.Name].message
		out = append(out, svc)
	}
	return out, nil
}

// DeleteBySession removes every service bound to a session. Returns the names
// deleted.
func (c *Client) DeleteBySession(ctx context.Context, session string) ([]string, error) {
	all, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, s := range all {
		if s.Session != session {
			continue
		}
		if ok, derr := c.Delete(ctx, s.Name); derr == nil && ok {
			deleted = append(deleted, s.Name)
		}
	}
	return deleted, nil
}

// Pause scales a service's Deployment to zero replicas, remembering the count
// it had (so Resume restores it). Idempotent: pausing an already-paused service
// keeps the originally remembered count. Returns the updated view.
func (c *Client) Pause(ctx context.Context, name string) (Service, error) {
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, err
	}
	if _, paused := d.Annotations[AnnoReplicasBeforePause]; !paused {
		prev := int32(0)
		if d.Spec.Replicas != nil {
			prev = *d.Spec.Replicas
		}
		if prev < 1 {
			prev = 1
		}
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations[AnnoReplicasBeforePause] = fmt.Sprint(prev)
	}
	zero := int32(0)
	d.Spec.Replicas = &zero
	if _, err := c.cs.AppsV1().Deployments(c.namespace).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		return Service{}, err
	}
	return c.Get(ctx, name)
}

// Resume restores a paused service to the replica count remembered at pause
// time (default 1 when absent) and clears the pause marker. Returns the
// updated view.
func (c *Client) Resume(ctx context.Context, name string) (Service, error) {
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, err
	}
	reps := int32(1)
	if v := d.Annotations[AnnoReplicasBeforePause]; v != "" {
		if n, perr := strconv.ParseInt(v, 10, 32); perr == nil && n > 0 {
			reps = int32(n)
		}
	}
	d.Spec.Replicas = &reps
	delete(d.Annotations, AnnoReplicasBeforePause)
	if _, err := c.cs.AppsV1().Deployments(c.namespace).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		return Service{}, err
	}
	return c.Get(ctx, name)
}

// Delete removes the Deployment + its Service(s) (primary + siblings).
func (c *Client) Delete(ctx context.Context, name string) (bool, error) {
	_, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	// Delete every Service bound to this deployment (primary + siblings).
	svcs, lerr := c.cs.CoreV1().Services(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=1," + LabelName + "=" + name})
	if lerr == nil {
		for i := range svcs.Items {
			_ = c.cs.CoreV1().Services(c.namespace).Delete(ctx, svcs.Items[i].Name, metav1.DeleteOptions{})
		}
	}
	if err := c.cs.AppsV1().Deployments(c.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}
