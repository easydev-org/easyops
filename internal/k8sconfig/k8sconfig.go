// Package k8sconfig builds the REST config for the cluster clients (sandboxmgr,
// servicesmgr).
//
// easyops uses an EXPLICIT kubeconfig from EASYOPS_KUBECONFIG, ALWAYS — no other
// source is consulted, so a deployment can run easyops in one namespace but act
// with the identity of another:
//
//	EASYOPS_KUBECONFIG -> (else) error
//
// Kubernetes forbids a Pod from referencing a ServiceAccount in another
// namespace, so when the identity/permissions live in namespace B while the Pod
// runs in namespace A, the deployment injects a kubeconfig (whose identity is
// namespace B's) and easyops uses it. The target namespace easyops operates in
// is orthogonal and still comes from SANDBOX_NAMESPACE.
//
// There is NO fallback of any kind: neither KUBECONFIG nor the in-cluster
// ServiceAccount is used. If EASYOPS_KUBECONFIG is unset, RESTConfig FAILS. A
// deployment must also NOT mount a ServiceAccount token (see the deploy chart).
package k8sconfig

import (
	"fmt"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// EnvKubeconfig is the ONLY environment variable easyops reads for its cluster
// identity. KUBECONFIG and the in-cluster ServiceAccount are deliberately NOT
// consulted.
const EnvKubeconfig = "EASYOPS_KUBECONFIG"

// RESTConfig returns the REST config for the cluster clients, read ONLY from
// EASYOPS_KUBECONFIG:
//
//	EASYOPS_KUBECONFIG -> (else) error
//
// There is no KUBECONFIG and no in-cluster fallback: a missing EASYOPS_KUBECONFIG
// is a hard error, so easyops can never silently act as the Pod's
// ServiceAccount or pick up an ambient KUBECONFIG.
func RESTConfig() (*rest.Config, error) {
	path := os.Getenv(EnvKubeconfig)
	if path == "" {
		return nil, fmt.Errorf("k8sconfig: %s is not set; easyops requires an explicit kubeconfig (KUBECONFIG and the in-cluster ServiceAccount are NOT used)", EnvKubeconfig)
	}
	rc, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("k8sconfig: %s %q set but unusable: %w", EnvKubeconfig, path, err)
	}
	return rc, nil
}

// Source reports which config source RESTConfig will use, for startup logging:
//
//	"EASYOPS_KUBECONFIG=<path>" | "none"
//
// It is read-only (it inspects the environment and builds no client), so a
// startup log can confirm which source is in effect.
func Source() string {
	if path := os.Getenv(EnvKubeconfig); path != "" {
		return EnvKubeconfig + "=" + path
	}
	return "none"
}
