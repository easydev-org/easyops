package opssvc

import (
	"context"
	"crypto/subtle"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/easy-platform/easyops/internal/platform/identity"
)

// IdentityVerifier resolves a bearer token to an identity. It is satisfied by
// *identity.Client and is an interface so the interceptor is testable without a
// network. (nil = no user-token validation configured.)
type IdentityVerifier interface {
	Whoami(ctx context.Context, token string) (identity.Identity, error)
}

// NewAuthInterceptor returns a Connect interceptor that requires
// `Authorization: Bearer <token>` on every RPC (unary and streaming). A missing
// or invalid token yields CodeUnauthenticated (HTTP 401).
//
// Two token kinds are accepted:
//  1. the shared SERVICE token - the cluster-internal caller (the gateway). It
//     is a trusted caller: any `owner` is allowed.
//  2. a valid USER token, verified against EasyVCS via `id` (see
//     EASYOPS_EASYVCS_URL). On this path the request's `owner` MUST equal the
//     token's username (defense in depth); a mismatch yields
//     CodePermissionDenied (HTTP 403).
//
// The verified identity is put on the context (see identityFrom) so handlers can
// tell the two paths apart. `id` may be nil (user tokens then rejected).
func NewAuthInterceptor(token string, id IdentityVerifier) connect.ServerInterceptor {
	a := &authInterceptor{token: token, id: id}
	return a.wrap
}

type authInterceptor struct {
	token string
	id    IdentityVerifier
}

// wrap authenticates every call, then wraps the stream so each received request
// message is owner-checked (a v2 server interceptor cannot see the request
// message itself, so the check happens on the handler's first Receive).
func (a *authInterceptor) wrap(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		header := ""
		if info, ok := connect.CallInfoForServerContext(ctx); ok {
			header = info.RequestHeader().Get("Authorization")
		}
		ctx, err := a.authorize(ctx, header)
		if err != nil {
			return err
		}
		return next(ctx, spec, &authStream{ServerStream: stream, a: a, ctx: ctx})
	}
}

// authStream wraps a ServerStream to owner-check each received request message.
type authStream struct {
	connect.ServerStream
	a   *authInterceptor
	ctx context.Context
}

func (s *authStream) Receive(msg any) error {
	if err := s.ServerStream.Receive(msg); err != nil {
		return err
	}
	return s.a.checkMsgOwner(s.ctx, msg)
}

// checkMsgOwner enforces, for a USER token, that the request's owner is empty
// or equals the token's username. A no-op for the shared service token.
func (a *authInterceptor) checkMsgOwner(ctx context.Context, msg any) error {
	ai, ok := authFrom(ctx)
	if !ok || ai.service {
		return nil
	}
	owner := ownerOf(msg)
	if owner != "" && owner != ai.id.Username {
		return ownerMismatch(owner, ai.id.Username)
	}
	return nil
}

// authorize resolves the bearer token and, on the user path, checks the owner.
// It returns the (possibly identity-bearing) context.
func (a *authInterceptor) authorize(ctx context.Context, header string) (context.Context, error) {
	tok, ok := bearer(header)
	if !ok {
		return ctx, unauthorized()
	}
	// 1) The shared service token is a trusted caller (the gateway): no owner
	// restriction. Checked first so it cannot be shadowed by a user lookup.
	if a.token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) == 1 {
		return withAuth(ctx, authInfo{service: true, id: identity.Identity{Username: "service", Authenticated: true}}), nil
	}
	// 2) A user token, verified against EasyVCS.
	if a.id == nil {
		return ctx, unauthorized()
	}
	id, err := a.id.Whoami(ctx, tok)
	if err != nil {
		// Backend error: treat as unauthenticated; do not leak the upstream
		// error to the caller.
		return ctx, connect.NewError(connect.CodeUnauthenticated, "identity lookup failed")
	}
	if !id.Authenticated {
		return ctx, unauthorized()
	}
	// The owner is checked on Receive (see authStream.Receive).
	return withAuth(ctx, authInfo{id: id}), nil
}

func ownerMismatch(owner, username string) error {
	return connect.Errorf(connect.CodePermissionDenied,
		"owner %q does not match the authenticated identity %q", owner, username)
}

// bearer extracts the token from an Authorization header (exact "Bearer "
// prefix; the scheme is case-sensitive).
func bearer(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	return strings.TrimPrefix(header, prefix), true
}

// ownerOf reads the caller-supplied owner off a request message. Every generated
// OpsService request implements GetOwner; a message without it has no owner.
func ownerOf(msg any) string {
	type ownerGetter interface{ GetOwner() string }
	if g, ok := msg.(ownerGetter); ok {
		return g.GetOwner()
	}
	return ""
}

func unauthorized() error {
	return connect.NewError(connect.CodeUnauthenticated, "missing or invalid service token")
}

// authInfo is what the interceptor puts on the context: the verified identity
// plus whether it is the shared service token (a trusted caller).
type authInfo struct {
	id      identity.Identity
	service bool
}

// authCtxKey carries authInfo on the context.
type authCtxKey struct{}

func withAuth(ctx context.Context, a authInfo) context.Context {
	return context.WithValue(ctx, authCtxKey{}, a)
}

// authFrom returns the auth info placed by the auth interceptor. ok=false means
// the interceptor did not run (e.g. a direct handler call in a test), in which
// case callers should treat the request as trusted.
func authFrom(ctx context.Context) (authInfo, bool) {
	a, ok := ctx.Value(authCtxKey{}).(authInfo)
	return a, ok
}

// identityFrom returns the verified identity placed by the auth interceptor.
func identityFrom(ctx context.Context) (identity.Identity, bool) {
	a, ok := authFrom(ctx)
	return a.id, ok
}

// checkOwner enforces, for a USER token, that the request's owner is empty or
// equals the token's username. It is a no-op for the shared service token (a
// trusted caller) and when the interceptor did not run.
func checkOwner(ctx context.Context, owner string) error {
	a, ok := authFrom(ctx)
	if !ok || a.service {
		return nil
	}
	if owner != "" && owner != a.id.Username {
		return ownerMismatch(owner, a.id.Username)
	}
	return nil
}
