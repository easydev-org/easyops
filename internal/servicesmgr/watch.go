package servicesmgr

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// WatchDeployments returns a watch over every managed service Deployment.
func (c *Client) WatchDeployments(ctx context.Context) (watch.Interface, error) {
	return c.cs.AppsV1().Deployments(c.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: LabelManaged + "=1",
	})
}

// WatchPVCs returns a watch over every managed PersistentVolumeClaim.
func (c *Client) WatchPVCs(ctx context.Context) (watch.Interface, error) {
	return c.cs.CoreV1().PersistentVolumeClaims(c.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: LabelPVC + "=1",
	})
}
