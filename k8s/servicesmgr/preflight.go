package servicesmgr

import (
	"context"

	"github.com/easy-platform/easyops/k8s/k8scheck"
)

// Preflight probes the RBAC the service backend needs in its namespace: the
// Deployment/Service/PVC create+list+delete it uses to run long-lived services.
// It is NON-FATAL — a denial is returned for the caller to log, never enforced.
func (c *Client) Preflight(ctx context.Context) []k8scheck.Check {
	return k8scheck.Review(ctx, c.cs, c.namespace, []k8scheck.Probe{
		{APIGroup: "apps", Resource: "deployments", Verbs: []string{"create", "list", "delete"}},
		{Resource: "services", Verbs: []string{"create", "list", "delete"}},
		{Resource: "persistentvolumeclaims", Verbs: []string{"create", "list", "delete"}},
	})
}
