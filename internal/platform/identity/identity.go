// Package identity is the platform's ONE identity client. EasyVCS is the single
// identity authority (users / tokens / namespaces); every other service
// (artifact, easyops, agent-gateway) validates a bearer token by asking
// EasyVCS, so there is one permission model enforced in one place.
//
// The package is deliberately dependency-light: stdlib only. It is imported by
// services that must NOT import easyvcs itself.
package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultTTL is how long a /whoami result is cached. Short enough that a
// revoked token self-heals quickly, long enough that a burst of RPCs from one
// session makes a single round-trip.
const DefaultTTL = 45 * time.Second

// Identity is who a bearer token belongs to.
type Identity struct {
	ID            int64
	Username      string
	DisplayName   string
	Authenticated bool
}

// Client resolves tokens against an EasyVCS base URL.
type Client struct {
	base string
	hc   *http.Client
	ttl  time.Duration

	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	id  Identity
	exp time.Time
}

// Option customises a Client.
type Option func(*Client)

// WithHTTPClient sets the transport (for pooling / timeouts).
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// WithTTL sets the cache TTL (<=0 disables caching).
func WithTTL(ttl time.Duration) Option { return func(c *Client) { c.ttl = ttl } }

// New returns a Client for the EasyVCS base URL (e.g.
// "http://easyvcs-server.worker.svc.cluster.local").
func New(base string, opts ...Option) *Client {
	c := &Client{
		base:  strings.TrimRight(base, "/"),
		hc:    &http.Client{Timeout: 10 * time.Second},
		ttl:   DefaultTTL,
		cache: map[string]entry{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ErrUnauthenticated is returned by Require for an invalid token.
var ErrUnauthenticated = errors.New("identity: unauthenticated")

// Whoami resolves a token. An invalid/expired token yields an Identity with
// Authenticated=false and a nil error — callers choose the policy (use Require
// for the common "reject" policy).
func (c *Client) Whoami(ctx context.Context, token string) (Identity, error) {
	if token == "" {
		return Identity{}, nil
	}
	if c.ttl > 0 {
		if id, ok := c.get(token); ok {
			return id, nil
		}
	}
	id, err := c.fetch(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	if c.ttl > 0 {
		c.put(token, id)
	}
	return id, nil
}

// Require resolves a token and errors with ErrUnauthenticated when it is not a
// valid, enabled identity.
func (c *Client) Require(ctx context.Context, token string) (Identity, error) {
	id, err := c.Whoami(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	if !id.Authenticated {
		return Identity{}, ErrUnauthenticated
	}
	return id, nil
}

func (c *Client) fetch(ctx context.Context, token string) (Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/whoami", nil)
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := c.hc.Do(req)
	if err != nil {
		return Identity{}, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		var body struct {
			ID            int64  `json:"id"`
			Username      string `json:"username"`
			DisplayName   string `json:"display_name"`
			Authenticated bool   `json:"authenticated"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			return Identity{}, fmt.Errorf("identity: decode whoami: %w", err)
		}
		return Identity{ID: body.ID, Username: body.Username, DisplayName: body.DisplayName, Authenticated: body.Authenticated}, nil
	case http.StatusUnauthorized:
		return Identity{}, nil
	default:
		return Identity{}, fmt.Errorf("identity: whoami: unexpected status %d", res.StatusCode)
	}
}

func (c *Client) get(token string) (Identity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[token]
	if !ok || time.Now().After(e.exp) {
		return Identity{}, false
	}
	return e.id, true
}

func (c *Client) put(token string, id Identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[token] = entry{id: id, exp: time.Now().Add(c.ttl)}
	// Lazy sweep so the map cannot grow without bound under many tokens.
	if len(c.cache) > 4096 {
		now := time.Now()
		for k, e := range c.cache {
			if now.After(e.exp) {
				delete(c.cache, k)
			}
		}
	}
}
