package servicesmgr

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Scale sets a service's desired replica count (0 = scaled down). Unlike Pause
// it does not remember the previous count, and it clears any pause marker.
func (c *Client) Scale(ctx context.Context, name string, replicas int32) (Service, error) {
	if replicas < 0 {
		return Service{}, fmt.Errorf("replicas must be >= 0")
	}
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, err
	}
	if d.Annotations != nil {
		delete(d.Annotations, AnnoReplicasBeforePause)
	}
	d.Spec.Replicas = &replicas
	if _, err := c.cs.AppsV1().Deployments(c.namespace).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		return Service{}, err
	}
	return c.Get(ctx, name)
}
