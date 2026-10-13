// Package helmmgr renders a Helm chart from a repository (using the Helm SDK
// templating packages only) and applies the result to the managed namespace via
// a dynamic client, with a strict safety filter. Release state (revision
// history + rendered manifests) is kept in a ConfigMap per release.
//
// It deliberately does NOT use Helm's action/installer stack: that pulls in
// kubectl/oras/kustomize and grants far more power than a tenant-scoped deploy
// surface should have. Templating is delegated to Helm; apply, ownership and
// rollback are owned here.
package helmmgr

import (
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"github.com/easy-platform/easyops/internal/k8sconfig"
)

// Client renders + applies Helm charts in one namespace.
type Client struct {
	dyn       dynamic.Interface
	cs        kubernetes.Interface
	mapper    meta.RESTMapper
	namespace string
}

// Config configures a Client.
type Config struct{ Namespace string }

// New builds a client from the shared REST config (see internal/k8sconfig):
// EASYOPS_KUBECONFIG is REQUIRED, so a deployment can run in one namespace but
// act with another's identity.
func New(cfg Config) (*Client, error) {
	rc, err := k8sconfig.RESTConfig()
	if err != nil {
		return nil, fmt.Errorf("helmmgr: %w", err)
	}
	return NewForConfig(rc, cfg)
}

// NewForConfig builds a client from an explicit rest config.
func NewForConfig(rc *rest.Config, cfg Config) (*Client, error) {
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return nil, err
	}
	gr, err := restmapper.GetAPIGroupResources(dc)
	if err != nil {
		return nil, fmt.Errorf("helmmgr: discovery: %w", err)
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = "worker"
	}
	return &Client{dyn: dyn, cs: cs, mapper: restmapper.NewDiscoveryRESTMapper(gr), namespace: ns}, nil
}

// Object is one rendered + applied resource.
type Object struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// Revision is one stored release revision.
type Revision struct {
	Revision  int    `json:"revision"`
	Ref       string `json:"ref"`
	ChartPath string `json:"chartPath"`
	// ChartVersion/AppVersion are the Chart.yaml `version`/`appVersion` at this
	// revision (recorded so a list view can show them and a rollback restores
	// them). AppVersion may be empty.
	ChartVersion string   `json:"chartVersion,omitempty"`
	AppVersion   string   `json:"appVersion,omitempty"`
	Values       string   `json:"values"`
	CreatedAt    int64    `json:"createdAt"`
	Objects      []Object `json:"objects"`
	Manifest     string   `json:"manifest"`
}

// Release is a Helm release managed by the gateway.
type Release struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Creator   string `json:"creator"`
	Session   string `json:"session"`
	// OrgNS is the tenant-internal namespace (org segment of the creating
	// session); distinct from the K8s Namespace above.
	OrgNS     string `json:"orgNs,omitempty"`
	Ref       string `json:"ref"`
	ChartPath string `json:"chartPath"`
	// ChartVersion/AppVersion mirror the CURRENT revision's chart metadata.
	ChartVersion string     `json:"chartVersion,omitempty"`
	AppVersion   string     `json:"appVersion,omitempty"`
	Revision     int        `json:"revision"`
	Status       string     `json:"status"`
	UpdatedAt    int64      `json:"updatedAt"`
	History      []Revision `json:"history"`
}

const (
	releaseLabel    = "workspace/helm-release"
	releaseCMName   = "helm-release-"
	annoReleaseName = "workspace/helm-release-name"
)

// RenderOptions describes one render+apply request.
type RenderOptions struct {
	// Release name (DNS-1123 label).
	Release string
	// ChartPath is the repo-relative directory holding Chart.yaml.
	ChartPath string
	// ValuesYAML is the user-supplied values document (may be empty).
	ValuesYAML string
	// Ref is the git ref the chart came from (recorded for history).
	Ref string
	// OrgNS is the tenant-internal namespace (org segment of the session).
	OrgNS string
}

func validReleaseName(s string) bool {
	if s == "" || len(s) > 53 {
		return false
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return false
		}
		if (i == 0 || i == len(s)-1) && r == '-' {
			return false
		}
	}
	return true
}

var _ = strconv.Itoa
var _ = runtime.NewScheme
