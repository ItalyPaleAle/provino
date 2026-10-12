// Package auth authenticates the callers of this service: browsers presenting a JWT, backends presenting a JWT or an API key, or anyone at all when authentication is disabled for development
//
// Authentication only establishes who is calling and which scopes they hold
// What a browser upload may do is decided per request by the authorizer, so nothing about a caller's permissions beyond its scopes is read from its credentials
package auth

import (
	"context"
	"slices"
)

// Principal is an authenticated caller
type Principal struct {
	// Subject the caller authenticated as
	Subject string
	// Scopes the caller holds
	Scopes []string
	// How the caller authenticated, for logs: "jwt", "apikey" or "none"
	Method string
}

// HasScope reports whether the caller holds a scope
// An empty scope is held by every caller, which is how a deployment opts out of requiring one
func (p Principal) HasScope(scope string) bool {
	return scope == "" || slices.Contains(p.Scopes, scope)
}

type principalContextKey struct{}

// PrincipalFromContext returns the authenticated caller of the request being handled
// The second result is false on a request that did not go through the middleware, which a handler treats as a failure rather than as an anonymous caller
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	return p, ok
}

// WithPrincipal attaches an authenticated caller to a context, which is what the middleware does and what a test does to stand in for it
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}
