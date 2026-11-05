//go:build integration

// Package rpce2e drives the REAL easyops HTTP surface with a real Connect client
// against a live k3d cluster.
//
// Nothing here calls the Service's Go methods directly: every call goes over the
// wire (the same handler stack `cmd/easyops` builds), through the auth and
// observe interceptors. This is the only suite where the generated bindings, the
// interceptor chain, the owner filtering and the Connect->HTTP error mapping are
// exercised together. A unit test calling `svc.CreateSandbox(...)` proves none
// of that.
package rpce2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	connect "connectrpc.com/connect/v2"
	connecthttp "connectrpc.com/connect/v2/connecthttp"
	"k8s.io/client-go/kubernetes"

	"github.com/easy-platform/easyops/gen/easyops/v1/opsv1connect"
	"github.com/easy-platform/easyops/internal/opssvc"
	"github.com/easy-platform/easyops/k8s/buildreg"
	"github.com/easy-platform/easyops/k8s/jobmgr"
	"github.com/easy-platform/easyops/k8s/k8sconfig"
	"github.com/easy-platform/easyops/k8s/runtimeprofiles"
	"github.com/easy-platform/easyops/k8s/sandboxmgr"
	"github.com/easy-platform/easyops/k8s/servicesmgr"
)

// testToken is the shared service token the harness configures.
const testToken = "integration-service-token"

// envOr returns a value or a default.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// fixture returns a registry-local image reference (pushed by
// scripts/fixture-images.sh). The cluster registry is required: sandboxmgr pins
// imagePullPolicy=Always, so a Docker Hub tag that does not exist never starts.
func fixture(t *testing.T, repo string) string {
	t.Helper()
	reg := os.Getenv("EASYOPS_TEST_REGISTRY")
	if reg == "" {
		t.Skip("EASYOPS_TEST_REGISTRY is not set: run scripts/k3d-up.sh + scripts/fixture-images.sh")
	}
	return reg + "/easyops-fixtures/" + repo
}

// harness is a live easyops server plus clients bound to it.
type harness struct {
	URL    string
	Client opsv1connect.OpsServiceClient
	Anon   opsv1connect.OpsServiceClient
	Bad    opsv1connect.OpsServiceClient
	Svc    *opssvc.Service
	NS     string
}

// newHarness starts the SAME handler stack cmd/easyops builds (auth -> observe ->
// OpsService) over httptest, so the wire behaviour is the production behaviour.
func newHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("EASYOPS_KUBECONFIG") == "" && os.Getenv("KUBECONFIG") == "" {
		t.Skip("no cluster configured (set EASYOPS_KUBECONFIG or KUBECONFIG)")
	}
	ns := envOr("EASYOPS_TEST_NAMESPACE", "easyops-ci")

	rc, err := k8sconfig.RESTConfig()
	if err != nil {
		t.Fatalf("rest config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	svc := opssvc.New(opssvc.Deps{
		Sandbox:             sandboxmgr.NewWithClientset(cs, sandboxmgr.Config{Namespace: ns}),
		Services:            servicesmgr.NewWithClientset(cs, servicesmgr.Config{Namespace: ns}),
		Jobs:                jobmgr.NewWithClientset(cs, jobmgr.Config{Namespace: ns}),
		Builds:              buildreg.New(0),
		Runtime:             runtimeprofiles.Settings{},
		DefaultSandboxImage: fixture(t, "worker:1"),
		PVCDefaultSize:      "100Mi",
		ServiceLogTail:      200,
		// Enable the registry guard, as a real deployment does: this also proves
		// the fixture images pass it.
		ImageRegistryHost: os.Getenv("EASYOPS_TEST_REGISTRY"),
		SandboxOrg:        "easyops-fixtures",
	})

	server := connect.NewServer(
		opssvc.NewAuthInterceptor(testToken, nil),
		opssvc.NewObserveInterceptor(),
	)
	opsv1connect.RegisterOpsServiceHandler(server, svc)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mk := func(withToken string) opsv1connect.OpsServiceClient {
		transport := connecthttp.NewTransport(&http.Client{}, srv.URL)
		// connect v2's chainClientInterceptors dereferences every interceptor, so
		// a nil entry PANICS (it does not skip it). The anonymous client must be
		// built with NO interceptor argument at all.
		var c *connect.Client
		if withToken != "" {
			c = connect.NewClient(transport, bearer(withToken))
		} else {
			c = connect.NewClient(transport)
		}
		return opsv1connect.NewOpsServiceClient(c)
	}
	return &harness{
		URL:    srv.URL,
		Client: mk(testToken),
		Anon:   mk(""),
		Bad:    mk("wrong-token"),
		Svc:    svc,
		NS:     ns,
	}
}

// bearer attaches a token to every request.
func bearer(token string) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			ctx, info := connect.NewClientContext(ctx)
			info.RequestHeader().Set("Authorization", "Bearer "+token)
			return next(ctx, spec)
		}
	}
}
