package sandboxmgr

import (
	"context"

	"github.com/easy-platform/easyops/internal/k8scheck"
)

// Preflight probes the RBAC the sandbox backend needs in its namespace: the
// Pod/Service/Secret create+list+delete it uses to run sandboxes. It is
// NON-FATAL — a denial is returned for the caller to log, never enforced.
func (c *Client) Preflight(ctx context.Context) []k8scheck.Check {
	return k8scheck.Review(ctx, c.cs, c.namespace, []k8scheck.Probe{
		{Resource: "pods", Verbs: []string{"create", "list", "delete"}},
		{Resource: "pods/log", Verbs: []string{"get"}},
		{Resource: "services", Verbs: []string{"create", "list", "delete"}},
		{Resource: "secrets", Verbs: []string{"create", "delete"}},
		// events: read for SandboxLogs diagnostics (best-effort; a denial only
		// means events are omitted, the logs still work).
		{Resource: "events", Verbs: []string{"list"}},
	})
}
