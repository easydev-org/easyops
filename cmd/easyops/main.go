// Command easyops is the easyops ops SERVER: it exposes the ops library
// (sandbox/service/image lifecycle) as the Connect service
// easyops.v1.OpsService over the cluster network.
//
// It is the ONLY easyops component that needs Kubernetes permissions. The
// gateway calls it over the network with a shared service token and never links
// the library. easyops holds NO policy: it authenticates the caller with
// EASYOPS_SERVICE_TOKEN and stamps/filters by the opaque `owner` the caller
// passes — it never decides whether an owner is genuine.
//
// Every backend is built from env and is OPTIONAL: an unconfigured backend makes
// its RPCs return CodeUnavailable (the process still starts).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/easy-platform/easyops/internal/platform/identity"
	"github.com/easy-platform/easyops/internal/platform/obs"

	"github.com/easy-platform/easyops/gen/easyops/v1/opsv1connect"
	"github.com/easy-platform/easyops/internal/imagebuild"
	"github.com/easy-platform/easyops/internal/k8scheck"
	"github.com/easy-platform/easyops/internal/k8sconfig"
	"github.com/easy-platform/easyops/internal/opssvc"
	"github.com/easy-platform/easyops/internal/runtimeprofiles"
	"github.com/easy-platform/easyops/internal/sandboxmgr"
	"github.com/easy-platform/easyops/internal/sandboxreaper"
	"github.com/easy-platform/easyops/internal/servicesmgr"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envOrDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("warn: %s=%q is not a duration; using %s", k, v, def)
	}
	return def
}

func envOrInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envOrBoolPtr parses an on/off-ish env var, returning nil (use the default)
// when unset. Recognised true: 1/true/on/yes; false: 0/false/off/no (case
// insensitive).
func envOrBoolPtr(k string, def bool) *bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	if v == "" {
		return nil
	}
	switch v {
	case "1", "true", "on", "yes":
		b := true
		return &b
	case "0", "false", "off", "no":
		b := false
		return &b
	}
	log.Printf("warn: %s=%q is not a boolean; using default %v", k, v, def)
	return nil
}

func main() {
	listen := "0.0.0.0:" + envOr("PORT", "8080")
	sandboxNS := envOr("SANDBOX_NAMESPACE", "worker")
	serviceToken := os.Getenv("EASYOPS_SERVICE_TOKEN")
	if serviceToken == "" {
		log.Printf("warn: EASYOPS_SERVICE_TOKEN is not set; OpsService RPCs will reject every call (401)")
	}
	// User-token validation: delegate to EasyVCS (/whoami) via platform/identity.
	// Without EASYOPS_EASYVCS_URL only the shared service token is accepted.
	var idClient opssvc.IdentityVerifier
	if u := os.Getenv("EASYOPS_EASYVCS_URL"); u != "" {
		idClient = identity.New(u)
		log.Printf("identity: user-token validation via EasyVCS at %s", u)
	} else {
		log.Printf("warn: EASYOPS_EASYVCS_URL unset; only the shared service token is accepted (no user tokens)")
	}

	// The sandbox backend owns the Kubernetes lifecycle in-process.
	sbx, err := sandboxmgr.New(sandboxmgr.Config{
		Namespace:     sandboxNS,
		CPURequest:    os.Getenv("SANDBOX_CPU_REQUEST"),
		CPULimit:      os.Getenv("SANDBOX_CPU_LIMIT"),
		MemoryRequest: os.Getenv("SANDBOX_MEMORY_REQUEST"),
		MemoryLimit:   os.Getenv("SANDBOX_MEMORY_LIMIT"),
	})
	if err != nil {
		log.Fatalf("sandbox backend: %v", err)
	}
	runtime, err := runtimeprofiles.Load(os.Getenv("WORKSPACE_RUNTIME"))
	if err != nil {
		log.Fatalf("load runtime settings: %v", err)
	}

	// Optional backends. Each is constructed from env; a construction failure is
	// logged and the backend is left nil (its RPCs return CodeUnavailable).
	services, err := servicesmgr.New(servicesmgr.Config{
		Namespace:     sandboxNS,
		CPURequest:    os.Getenv("SERVICE_CPU_REQUEST"),
		CPULimit:      os.Getenv("SERVICE_CPU_LIMIT"),
		MemoryRequest: os.Getenv("SERVICE_MEMORY_REQUEST"),
		MemoryLimit:   os.Getenv("SERVICE_MEMORY_LIMIT"),
	})
	if err != nil {
		log.Printf("warn: services backend unavailable: %v", err)
		services = nil
	}
	if services == nil {
		log.Printf("warn: services backend is NIL: PVC/service RPCs are unavailable and golden_disk cannot create its shared cache PVC")
	}
	builder := newImageBuilder()
	svc := opssvc.New(opssvc.Deps{
		Sandbox:             sbx,
		Runtime:             runtime,
		DefaultSandboxImage: os.Getenv("DEFAULT_SANDBOX_IMAGE"),
		Services:            services,
		Builder:             builder,
		PVCStorageClass:     os.Getenv("PVC_STORAGE_CLASS"),
		PVCDefaultSize:      envOr("PVC_DEFAULT_SIZE", "1Gi"),
		GoldenDiskCachePVC:  os.Getenv("SANDBOX_GOLDEN_PVC"),
		GoldenDiskCacheSize: os.Getenv("SANDBOX_GOLDEN_PVC_SIZE"),
		ServiceLogTail:      int64(envOrInt("SERVICE_LOG_TAIL", 200)),
		// Sandbox package upstream: inject the artifact mirror's standard env
		// (env-var-addressable tools) into every sandbox and, unless disabled,
		// run the file-type bootstrap setup job.
		SandboxPackageUpstream: os.Getenv("SANDBOX_PACKAGE_UPSTREAM"),
		SandboxBootstrap:       envOrBoolPtr("SANDBOX_BOOTSTRAP", true),
		// Image guards: a sandbox image must come from artifact + SANDBOX_ORG; a
		// service image from artifact (any org). Both disabled when the host is
		// unset.
		SandboxOrg:        envOr("SANDBOX_ORG", "sandbox"),
		ImageRegistryHost: os.Getenv("IMAGE_REGISTRY_HOST"),
	})
	// Log the REST-config source + identity BEFORE any RBAC check, so an operator
	// can confirm EASYOPS_KUBECONFIG took effect (a missing/mistyped env shows as
	// source "none" and easyops cannot build a cluster client).
	logK8sIdentity()
	// NON-FATAL RBAC self-check: log LOUDLY when the Pod's ServiceAccount cannot
	// create/list/delete sandbox objects, so a missing worker-namespace
	// Role/RoleBinding is visible at boot (a healthy deployment logs no DENIED).
	runRBACPreflight(svc)

	// Reclaim idle sandboxes (no worker jobs for SANDBOX_IDLE_TTL).
	startSandboxReaper(sbx, sandboxNS)

	mux := http.NewServeMux()
	// easyops.v1.OpsService is the server's ONLY surface; every RPC requires the
	// shared service token (trusted caller) or a valid user token (verified
	// against EasyVCS, with owner == identity). Every RPC is observed on the
	// metrics registry.
	server := connect.NewServer(
		opssvc.NewAuthInterceptor(serviceToken, idClient),
		opssvc.NewObserveInterceptor(),
	)
	opsv1connect.RegisterOpsServiceHandler(server, svc)
	connecthttp.Mount(mux, server)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok " + appVersion()))
	})
	mux.HandleFunc("/readyz", readyz(svc))
	mountUI(mux)

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		Protocols:         protocols,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("easyops listening on %s (sandbox-ns=%s services=%v image=%v)",
		listen, sandboxNS, services != nil, builder != nil)

	// Prometheus /metrics on a SEPARATE port (METRICS_PORT, default 9090) so it
	// never contends with the OpsService/healthz mux on PORT.
	go func() {
		addr := "0.0.0.0:" + envOr("METRICS_PORT", "9090")
		log.Printf("metrics: /metrics on %s", addr)
		if err := obs.Default.Serve(addr); err != nil {
			log.Printf("warn: metrics server: %v", err)
		}
	}()
	log.Fatal(srv.ListenAndServe())
}

// newImageBuilder builds the image build/import backend from env. It returns nil
// (RPCs -> CodeUnavailable) unless BUILDKIT_ADDR and IMAGE_REGISTRY_HOST are set.
func newImageBuilder() *imagebuild.Builder {
	addr := os.Getenv("BUILDKIT_ADDR")
	host := os.Getenv("IMAGE_REGISTRY_HOST")
	if addr == "" || host == "" {
		log.Printf("image builder disabled (BUILDKIT_ADDR and/or IMAGE_REGISTRY_HOST unset)")
		return nil
	}
	return &imagebuild.Builder{
		Addr:           addr,
		RegistryHost:   host,
		RegistryScheme: envOr("IMAGE_REGISTRY_SCHEME", "http"),
		RegistryUser:   os.Getenv("IMAGE_REGISTRY_USER"),
		RegistryPass:   os.Getenv("IMAGE_REGISTRY_PASSWORD"),
	}
}

// startSandboxReaper launches the idle-sandbox reaper in the background. It is
// disabled when SANDBOX_IDLE_TTL <= 0. The concrete worker client lives in the
// service layer (opssvc) so the sandboxreaper library stays connectrpc-free.
func startSandboxReaper(sbx *sandboxmgr.Client, sandboxNS string) {
	ttl := envOrDuration("SANDBOX_IDLE_TTL", 24*time.Hour)
	if ttl <= 0 {
		log.Printf("sandbox reaper: disabled (SANDBOX_IDLE_TTL <= 0)")
		return
	}
	reaper := &sandboxreaper.Reaper{
		Sbx:       sbx,
		IdleTTL:   ttl,
		Interval:  envOrDuration("SANDBOX_REAP_INTERVAL", 10*time.Minute),
		NewWorker: func(url, token string) sandboxreaper.WorkerLister { return opssvc.NewWorkerClient(url, token) },
	}
	go reaper.Run(context.Background())
	log.Printf("sandbox reaper: started (ns=%s idle TTL=%s)", sandboxNS, ttl)
}

// readyz reports whether the server can reach the Kubernetes API. It runs the
// sandbox backend's RBAC self-check; an unreachable API server (every probe
// failing to even perform the review) is NOT ready. A mere RBAC denial is
// reachable-but-misconfigured: the server is ready (it serves and returns the
// API server's own errors) but a warning is logged. Never blocks startup.
// The RBAC-denial warning is EDGE-TRIGGERED (see rbacWarnGate): kubelet probes
// /readyz every few seconds, so an unconditional warn would flood the log while
// RBAC is unconfigured (a single log tail becomes almost all warning lines).
// The first denial is reported, later probes stay silent, and a recovery
// re-arms it (logging one "cleared" line).
func readyz(svc *opssvc.Service) http.HandlerFunc {
	gate := &rbacWarnGate{}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		checks := svc.Preflight(ctx)
		reachable := false
		for _, c := range checks {
			if c.Allowed || !isReviewFailure(c.Reason) {
				reachable = true
			}
		}
		if !reachable {
			http.Error(w, "kubernetes API unreachable", http.StatusServiceUnavailable)
			return
		}
		if denied := k8scheck.Denied(checks); len(denied) > 0 {
			gate.warnOnce(func() {
				log.Printf("warn: readyz: %d RBAC permission(s) DENIED in namespace %q — operations will fail with 403", len(denied), denied[0].Namespace)
			})
		} else if gate.clear() {
			log.Printf("readyz: RBAC denials cleared")
		}
		_, _ = w.Write([]byte("ok"))
	}
}

// isReviewFailure reports whether a check's reason is a failed SelfSubjectAccess
// Review (API unreachable), as opposed to a definitive RBAC denial.
func isReviewFailure(reason string) bool {
	const prefix = "review failed:"
	return len(reason) >= len(prefix) && reason[:len(prefix)] == prefix
}

// runRBACPreflight logs the server's Kubernetes RBAC self-check at boot. It is
// deliberately NON-FATAL: a denial is visible in the boot log instead of only on
// the first failed operation.
func runRBACPreflight(svc *opssvc.Service) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checks := svc.Preflight(ctx)
	for _, line := range k8scheck.Summary(checks) {
		log.Printf("%s", line)
	}
	if denied := k8scheck.Denied(checks); len(denied) > 0 {
		log.Printf("warn: rbac: %d permission(s) DENIED — sandbox operations will fail with 403. "+
			"Fix: (1) the Pod MUST run as the easyops ServiceAccount; (2) the worker-namespace Role/RoleBinding MUST be applied (k8s/rbac.yaml).",
			len(denied))
	}
}

// logK8sIdentity prints ONE line describing the REST-config source and the
// identity the cluster clients will act as, so an operator can confirm an
// injected kubeconfig took effect (a missing/mistyped env shows up as source
// "none"). It is best-effort and NEVER blocks startup: an identity that cannot
// be resolved is logged as "unknown".
func logK8sIdentity() {
	rc, err := k8sconfig.RESTConfig()
	if err != nil {
		log.Printf("k8sconfig: source=%s (could not build REST config: %v)", k8sconfig.Source(), err)
		return
	}
	identity := "unknown"
	if cs, cerr := kubernetes.NewForConfig(rc); cerr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if res, rerr := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx,
			&authv1.SelfSubjectReview{}, metav1.CreateOptions{}); rerr == nil {
			if u := res.Status.UserInfo.Username; u != "" {
				identity = u
			}
		} else {
			identity = "unknown (" + rerr.Error() + ")"
		}
	}
	log.Printf("k8sconfig: using %s (server=%s, identity=%s)", k8sconfig.Source(), rc.Host, identity)
}
