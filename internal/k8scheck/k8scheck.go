// Package k8scheck runs a NON-FATAL startup self-check of the gateway's
// Kubernetes RBAC.
//
// The gateway serves VCS/session routes even when the cluster is unreachable or
// its ServiceAccount lacks permissions — that is deliberate (a k8s outage must
// not take the whole gateway down). The downside is that a missing Role or
// RoleBinding stays invisible until someone tries to create a sandbox (403) or
// notices a per-sweep reaper warning. This package closes that gap: at boot the
// gateway asks the API server, via SelfSubjectAccessReview, whether it may
// perform the operations the sandbox/service backends need, and the caller logs
// a loud warning for every denial.
//
// A denial here never changes behaviour: the gateway still attempts the real
// operation and returns whatever the API server says.
package k8scheck

import (
	"context"
	"fmt"
	"strings"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Check is one (resource, verb) probe result.
type Check struct {
	// Resource is the probed resource, e.g. "pods" or "deployments.apps".
	Resource string
	// Verb is the probed verb (create | list | delete | ...).
	Verb string
	// Namespace is the namespace the probe was scoped to.
	Namespace string
	// Allowed is the API server's verdict.
	Allowed bool
	// Reason explains a denial (empty when allowed, or when the server gave
	// none).
	Reason string
}

// Probe is one resource and the verbs to check on it.
type Probe struct {
	// APIGroup is the resource's API group ("" for the core group).
	APIGroup string
	// Resource is the plural resource name, e.g. "pods".
	Resource string
	// Verbs are the verbs to probe.
	Verbs []string
}

// Label renders a probe's resource as "<resource>" or "<resource>.<group>"
// (matching kubectl's `resource.group` form, e.g. "deployments.apps").
func (p Probe) Label() string {
	if p.APIGroup == "" {
		return p.Resource
	}
	return p.Resource + "." + p.APIGroup
}

// Review runs one SelfSubjectAccessReview per (probe, verb) in namespace ns and
// returns every result in probe order. A review that cannot be performed (API
// unreachable, unexpected error) is reported as a denial carrying the error, so
// a broken cluster surfaces the same way as a missing permission.
func Review(ctx context.Context, cs kubernetes.Interface, ns string, probes []Probe) []Check {
	out := make([]Check, 0, len(probes))
	for _, p := range probes {
		for _, v := range p.Verbs {
			out = append(out, review(ctx, cs, ns, p, v))
		}
	}
	return out
}

// review performs one SelfSubjectAccessReview.
func review(ctx context.Context, cs kubernetes.Interface, ns string, p Probe, verb string) Check {
	c := Check{Resource: p.Label(), Verb: verb, Namespace: ns}
	attrs := &authv1.ResourceAttributes{Namespace: ns, Verb: verb, Resource: p.Resource}
	if p.APIGroup != "" {
		attrs.Group = p.APIGroup
	}
	res, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx,
		&authv1.SelfSubjectAccessReview{
			Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: attrs},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		c.Reason = "review failed: " + err.Error()
		return c
	}
	c.Allowed = res.Status.Allowed
	if !c.Allowed {
		c.Reason = strings.TrimSpace(res.Status.Reason)
		if c.Reason == "" {
			c.Reason = strings.TrimSpace(res.Status.EvaluationError)
		}
		if c.Reason == "" {
			c.Reason = "denied by RBAC (no matching Role/RoleBinding for this ServiceAccount)"
		}
	}
	return c
}

// Denied returns only the denied checks, in order.
func Denied(checks []Check) []Check {
	out := make([]Check, 0, len(checks))
	for _, c := range checks {
		if !c.Allowed {
			out = append(out, c)
		}
	}
	return out
}

// Summary renders one line per check, e.g.
//
//	rbac: pods/create in ns "worker": allowed
//	rbac: deployments.apps/list in ns "worker": DENIED (no matching Role/RoleBinding ...)
//
// It is used for the boot log and by tests.
func Summary(checks []Check) []string {
	lines := make([]string, 0, len(checks))
	for _, c := range checks {
		if c.Allowed {
			lines = append(lines, fmt.Sprintf("rbac: %s/%s in ns %q: allowed", c.Resource, c.Verb, c.Namespace))
			continue
		}
		lines = append(lines, fmt.Sprintf("rbac: %s/%s in ns %q: DENIED (%s)", c.Resource, c.Verb, c.Namespace, c.Reason))
	}
	return lines
}
