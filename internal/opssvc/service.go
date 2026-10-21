// Package opssvc is the easyops SERVICE ADAPTER layer: it maps the Connect
// easyops.v1.OpsService RPCs onto the ops library packages (sandboxmgr,
// servicesmgr, imagebuild).
//
// It is the ONLY library-adjacent package allowed to import connectrpc (see
// DEVELOP.md §2). It holds NO policy: it validates parameters, enforces the
// shared service token (see auth.go), stamps the caller-supplied `owner` onto
// created objects and filters List*/Get*/Delete* by it. It never decides whether
// an owner is genuine — that is the gateway's concern.
//
// Every backend is OPTIONAL: an unconfigured backend makes its RPCs return
// CodeUnavailable instead of panicking (a deployment may expose only a subset).
package opssvc

import (
	"context"
	"time"

	"github.com/easy-platform/easyops/gen/easyops/v1/opsv1connect"
	"github.com/easy-platform/easyops/k8s/buildreg"
	"github.com/easy-platform/easyops/k8s/imagebuild"
	"github.com/easy-platform/easyops/k8s/jobmgr"
	"github.com/easy-platform/easyops/k8s/k8scheck"
	"github.com/easy-platform/easyops/k8s/runtimeprofiles"
	"github.com/easy-platform/easyops/k8s/sandboxmgr"
	"github.com/easy-platform/easyops/k8s/servicesmgr"
)

// serviceReadyTimeout bounds how long DeployService waits for a freshly-deployed
// pod to become ready. A deterministic failure returns immediately; a genuine
// slow start is reported as not-ready (non-fatal).
const serviceReadyTimeout = 60 * time.Second

// Service implements easyops.v1.OpsService over the ops library.
type Service struct {
	sbx          *sandboxmgr.Client
	runtime      runtimeprofiles.Settings
	defaultImage string

	services *servicesmgr.Client
	builder  *imagebuild.Builder
	// builds is the in-memory registry behind CreateBuild/ListBuilds/GetBuild/
	// CancelBuild (nil = those RPCs are unavailable).
	builds *buildreg.Registry
	// jobs is the run-to-completion (batch/v1.Job) backend (nil = unavailable).
	jobs *jobmgr.Client

	pvcStorageClass string
	pvcDefaultSize  string
	// goldenDiskCachePVC / goldenDiskCacheSize configure the shared golden-disk
	// cache PVC mounted by VM (Windows/macOS) sandboxes. Empty name = the
	// default; empty size = "40Gi".
	goldenDiskCachePVC  string
	goldenDiskCacheSize string
	serviceLogTail      int64

	// sandboxPackageUpstream is the artifact base URL injected into every
	// sandbox as its package-manager upstream (empty = inject nothing).
	sandboxPackageUpstream string
	// sandboxBootstrap gates the FILE-type bootstrap setup job (default on; only
	// runs when sandboxPackageUpstream is set).
	sandboxBootstrap bool
	// sandboxOrg is the ONLY registry org a sandbox image may come from (empty =
	// the org guard is disabled).
	sandboxOrg string
	// imageRegistryHost is the artifact registry host every sandbox/service image
	// must come from (empty = the registry guard is disabled).
	imageRegistryHost string
	// newWorker builds the worker client used by the bootstrap (and the reaper).
	// It is a seam so the library layer never imports the worker protocol.
	newWorker func(url, token string) workerExecutor
}

// Deps configures a Service. Every backend except Sandbox is optional: a nil
// backend makes its RPCs return CodeUnavailable.
type Deps struct {
	// Sandbox is the (already constructed) sandbox backend.
	Sandbox *sandboxmgr.Client
	// Runtime is the rendered runtime settings (KVM/GPU knobs).
	Runtime runtimeprofiles.Settings
	// DefaultSandboxImage is used when CreateSandbox omits an image.
	DefaultSandboxImage string
	// Services is the long-lived-service + PVC backend (nil = unavailable).
	Services *servicesmgr.Client
	// Builder is the image build/import backend (nil = unavailable).
	Builder *imagebuild.Builder
	// Builds is the in-memory registry for asynchronous builds (nil =
	// CreateBuild/ListBuilds/GetBuild/CancelBuild are unavailable).
	Builds *buildreg.Registry
	// Jobs is the run-to-completion (batch/v1.Job) backend (nil = unavailable).
	Jobs *jobmgr.Client
	// PVCStorageClass is the deployment's storage class for CreatePVC (empty =
	// the caller may pass any class).
	PVCStorageClass string
	// PVCDefaultSize is used when CreatePVC omits a size (empty = "1Gi").
	PVCDefaultSize string
	// GoldenDiskCachePVC / GoldenDiskCacheSize configure the shared golden-disk
	// cache PVC mounted by VM (Windows/macOS) sandboxes (empty = default name /
	// "40Gi").
	GoldenDiskCachePVC  string
	GoldenDiskCacheSize string
	// ServiceLogTail is the default tail line count for ServiceLogs (0 = all).
	ServiceLogTail int64
	// SandboxPackageUpstream is the artifact base URL injected into every
	// sandbox as its package-manager upstream (empty = inject nothing).
	SandboxPackageUpstream string
	// SandboxBootstrap gates the FILE-type bootstrap setup job. Nil = default
	// (ON, but only when SandboxPackageUpstream is set).
	SandboxBootstrap *bool
	// SandboxOrg is the ONLY registry org a sandbox image may come from (empty =
	// the org guard is disabled).
	SandboxOrg string
	// ImageRegistryHost is the artifact registry host every sandbox/service image
	// must come from (empty = the registry guard is disabled).
	ImageRegistryHost string
	// NewWorker builds the worker client for the bootstrap. Nil = the concrete
	// WorkerClient (the library layer never imports the worker protocol).
	NewWorker func(url, token string) workerExecutor
}

// New builds a Service.
func New(d Deps) *Service {
	bootstrap := true
	if d.SandboxBootstrap != nil {
		bootstrap = *d.SandboxBootstrap
	}
	newWorker := d.NewWorker
	if newWorker == nil {
		newWorker = func(url, token string) workerExecutor { return NewWorkerClient(url, token) }
	}
	return &Service{
		sbx:                    d.Sandbox,
		runtime:                d.Runtime,
		defaultImage:           d.DefaultSandboxImage,
		services:               d.Services,
		jobs:                   d.Jobs,
		builder:                d.Builder,
		builds:                 d.Builds,
		pvcStorageClass:        d.PVCStorageClass,
		pvcDefaultSize:         d.PVCDefaultSize,
		goldenDiskCachePVC:     d.GoldenDiskCachePVC,
		goldenDiskCacheSize:    d.GoldenDiskCacheSize,
		serviceLogTail:         d.ServiceLogTail,
		sandboxPackageUpstream: d.SandboxPackageUpstream,
		sandboxBootstrap:       bootstrap,
		sandboxOrg:             d.SandboxOrg,
		imageRegistryHost:      d.ImageRegistryHost,
		newWorker:              newWorker,
	}
}

// compile-time assertion that the adapter implements the generated handler.
var _ opsv1connect.OpsServiceHandler = (*Service)(nil)

// Preflight exposes the sandbox backend's non-fatal RBAC self-check, so the
// server's /readyz can probe k8s reachability (see cmd/easyops).
func (s *Service) Preflight(ctx context.Context) []k8scheck.Check { return s.sbx.Preflight(ctx) }

// owned reports whether an object created by `creator` is visible to `owner`. An
// empty owner means "no filter"; an unowned object (creator "") is visible to
// everyone. This is a filter, NOT an authorization check.
func owned(creator, owner string) bool {
	return owner == "" || creator == "" || creator == owner
}

// writable reports whether `owner` may MUTATE an existing object created by
// `creator`. Unlike `owned` (a read filter), an UNOWNED object (creator "") is
// NOT writable by anyone: a missing creator cannot be proven to belong to the
// caller, so it must be explicitly adopted first. (A1 policy.)
func writable(creator, owner string) bool {
	return creator != "" && creator == owner
}
