package opssvc

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/easy-platform/easyops/internal/platform/obs"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/gen/easyops/v1/opsv1connect"
)

// observeTestServer starts a real Connect server with the observe interceptor and
// returns a client + a way to render the metrics.
func observeTestServer(t *testing.T) (opsv1connect.OpsServiceClient, *obs.Registry) {
	t.Helper()
	reg := obs.NewRegistry()
	prev := obs.Default
	obs.Default = reg
	t.Cleanup(func() { obs.Default = prev })

	s := newTestService()
	server := connect.NewServer(NewObserveInterceptor())
	opsv1connect.RegisterOpsServiceHandler(server, s)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return opsv1connect.NewOpsServiceClient(connect.NewClient(connecthttp.NewTransport(srv.Client(), srv.URL, connecthttp.WithReadMaxBytes(0)))), reg
}

// TestObserveInterceptorRecords checks that one RPC produces a request counter
// and a latency histogram labelled by the real procedure + code.
func TestObserveInterceptorRecords(t *testing.T) {
	client, reg := observeTestServer(t)
	if _, err := client.ListSandboxes(context.Background(),
		&opsv1.ListSandboxesRequest{Owner: "a"}); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	reg.Render(&b)
	out := b.String()
	if !strings.Contains(out, `rpc_requests_total{code="ok",proc="/easyops.v1.OpsService/ListSandboxes"} 1`) {
		t.Fatalf("counter missing:\n%s", out)
	}
	if !strings.Contains(out, `rpc_duration_seconds_count{proc="/easyops.v1.OpsService/ListSandboxes"} 1`) {
		t.Fatalf("histogram missing:\n%s", out)
	}
}

// TestObserveInterceptorErrorCode checks a failing RPC is labelled with its
// Connect code (DeployService on an unconfigured backend -> unavailable).
func TestObserveInterceptorErrorCode(t *testing.T) {
	client, reg := observeTestServer(t)
	_, err := client.DeployService(context.Background(),
		&opsv1.DeployServiceRequest{Owner: "a", Image: "i"})
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v, want Unavailable", connect.CodeOf(err))
	}
	var b bytes.Buffer
	reg.Render(&b)
	if !strings.Contains(b.String(), `rpc_requests_total{code="unavailable",proc="/easyops.v1.OpsService/DeployService"} 1`) {
		t.Fatalf("error code not labelled:\n%s", b.String())
	}
}
