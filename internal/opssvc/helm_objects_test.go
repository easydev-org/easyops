package opssvc

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
)

// TestHelmObjectsUnavailable: with no helm backend, HelmObjects/HelmObjectLogs
// return CodeUnavailable (never panic).
func TestHelmObjectsUnavailable(t *testing.T) {
	s := newTestService() // no helm backend
	ctx := context.Background()
	if _, err := s.HelmObjects(ctx, &opsv1.HelmObjectsRequest{Owner: "a", Release: "r"}); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("HelmObjects: code = %v, want Unavailable", connect.CodeOf(err))
	}
	if _, err := s.HelmObjectLogs(ctx, &opsv1.HelmObjectLogsRequest{Owner: "a", Release: "r", Kind: "Pod", Name: "p"}); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("HelmObjectLogs: code = %v, want Unavailable", connect.CodeOf(err))
	}
}
