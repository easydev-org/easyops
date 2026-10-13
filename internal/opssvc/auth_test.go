package opssvc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/easy-platform/easyops/internal/platform/identity"

	"github.com/easy-platform/easyops/gen/easyops/v1/opsv1connect"
)

// fakeVerifier is an identityVerifier returning canned results per token.
type fakeVerifier struct {
	byToken map[string]identity.Identity
	err     error
}

func (f *fakeVerifier) Whoami(_ context.Context, token string) (identity.Identity, error) {
	if f.err != nil {
		return identity.Identity{}, f.err
	}
	return f.byToken[token], nil
}

// TestAuthInterceptor401 verifies that a missing or wrong bearer token is
// rejected with CodeUnauthenticated before the handler runs, and that the
// correct service token is accepted.
func TestAuthInterceptor401(t *testing.T) {
	s := newTestService()
	server := connect.NewServer(NewAuthInterceptor("s3cret", nil))
	opsv1connect.RegisterOpsServiceHandler(server, s)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	methodURL := srv.URL + "/easyops.v1.OpsService/CreateSandbox"

	client := &http.Client{}
	body := `{"owner":"a","name":"x","image":"img:1"}`
	const ct = "application/json"

	// No token -> 401.
	req, _ := http.NewRequest(http.MethodPost, methodURL, strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Wrong token -> 401.
	req, _ = http.NewRequest(http.MethodPost, methodURL, strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer nope")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Correct service token -> 200.
	req, _ = http.NewRequest(http.MethodPost, methodURL, strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct token: status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBearerParsing(t *testing.T) {
	cases := map[string]struct {
		tok string
		ok  bool
	}{
		"Bearer tok":  {"tok", true},
		"Bearer  tok": {" tok", true}, // extra space is part of the token
		"bearer tok":  {"", false},    // scheme is case-sensitive
		"tok":         {"", false},
		"":            {"", false},
	}
	for h, want := range cases {
		got, ok := bearer(h)
		if got != want.tok || ok != want.ok {
			t.Errorf("bearer(%q) = (%q,%v), want (%q,%v)", h, got, ok, want.tok, want.ok)
		}
	}
}

// TestUserTokenOwnerMatchAccepted: a valid user token with owner == username is
// accepted.
func TestUserTokenOwnerMatchAccepted(t *testing.T) {
	v := &fakeVerifier{byToken: map[string]identity.Identity{
		"user-tok": {ID: 7, Username: "alice", Authenticated: true},
	}}
	a := &authInterceptor{token: "svc", id: v}
	ctx, err := a.authorize(context.Background(), "Bearer user-tok")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if id, ok := identityFrom(ctx); !ok || id.Username != "alice" {
		t.Fatalf("identity not attached: %+v ok=%v", id, ok)
	}
}

// TestUserTokenOwnerMismatch403: owner != username is rejected with
// CodePermissionDenied.
func TestUserTokenOwnerMismatch403(t *testing.T) {
	v := &fakeVerifier{byToken: map[string]identity.Identity{
		"user-tok": {Username: "alice", Authenticated: true},
	}}
	a := &authInterceptor{token: "svc", id: v}
	ctx, err := a.authorize(context.Background(), "Bearer user-tok")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	err = a.checkMsgOwner(ctx, &req{owner: "bob"})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", connect.CodeOf(err))
	}
}

// TestUserTokenInvalid401: an unauthenticated user token is 401.
func TestUserTokenInvalid401(t *testing.T) {
	v := &fakeVerifier{byToken: map[string]identity.Identity{}}
	a := &authInterceptor{token: "svc", id: v}
	_, err := a.authorize(context.Background(), "Bearer nope")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

// TestUserTokenWithoutVerifier401: with no EasyVCS configured, a user token is
// rejected (only the service token is accepted).
func TestUserTokenWithoutVerifier401(t *testing.T) {
	a := &authInterceptor{token: "svc", id: nil}
	_, err := a.authorize(context.Background(), "Bearer user-tok")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

// TestServiceTokenBypassesOwner: the shared service token may carry any owner.
func TestServiceTokenBypassesOwner(t *testing.T) {
	a := &authInterceptor{token: "svc", id: nil}
	if _, err := a.authorize(context.Background(), "Bearer svc"); err != nil {
		t.Fatalf("service token must not be owner-restricted: %v", err)
	}
}

// TestIdentityLookupErrorIs401: a backend error during lookup must not leak and
// must be unauthenticated.
func TestIdentityLookupErrorIs401(t *testing.T) {
	v := &fakeVerifier{err: errors.New("easyvcs down")}
	a := &authInterceptor{token: "svc", id: v}
	_, err := a.authorize(context.Background(), "Bearer user-tok")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
	if strings.Contains(err.Error(), "easyvcs down") {
		t.Fatalf("upstream error leaked: %v", err)
	}
}

// req is a minimal GetOwner-bearing message for the authorize unit tests.
type req struct{ owner string }

func (r *req) GetOwner() string { return r.owner }
