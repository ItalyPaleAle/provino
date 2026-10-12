package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/italypaleale/provino/internal/auth"
	"github.com/italypaleale/provino/internal/config"
)

// RateLimiter is the Francis built-in rate-limit service used by the HTTP layer
type RateLimiter interface {
	Allow(ctx context.Context, key string) (allowed bool, retryAfter time.Duration, err error)
}

// middlewareRateLimit rejects excess requests before their upload bodies are read or stored
// It runs after authentication, so the budget belongs to the caller rather than to an address that a whole office can share
func (s *Server) middlewareRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Route the per-caller decision through Francis before reading the upload body
		key := rateLimitSubject(r)
		allowed, retryAfter, err := s.rateLimiter.Allow(r.Context(), key)
		if err != nil {
			slog.ErrorContext(r.Context(), "Failed to check the ingest rate limit", slog.Any("error", err))
			errInternal.WriteResponse(w, r)
			return
		}

		if !allowed {
			// Retry-After uses whole seconds and must never round a positive wait down to zero
			seconds := max(1, int(math.Ceil(retryAfter.Seconds())))
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			errRateLimited.WriteResponse(w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// rateLimitSubject keys the budget on the authenticated caller, falling back to the client's address for a caller without a subject
// The subject is hashed, so a key never repeats an identifier from a token in the actor store
func rateLimitSubject(r *http.Request) string {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if ok && principal.Subject != "" {
		digest := sha256.Sum256([]byte(principal.Subject))
		return "sub-" + hex.EncodeToString(digest[:16])
	}

	return "ip-" + clientIP(r, config.Get().Server.TrustedProxies)
}

// clientIP trusts forwarded client headers only when the direct peer is an explicitly configured proxy
func clientIP(r *http.Request, trustedProxies []string) string {
	// Forwarded addresses are authoritative only when a configured proxy supplied them
	if requestFromTrustedProxy(r, trustedProxies) {
		forwardedFor := r.Header.Get("X-Forwarded-For")
		if forwardedFor != "" {
			client, _, _ := strings.Cut(forwardedFor, ",")
			client = strings.TrimSpace(client)
			if client != "" {
				return rateLimitKey(client)
			}
		}

		realIP := strings.TrimSpace(r.Header.Get("X-Real-IP"))
		if realIP != "" {
			return rateLimitKey(realIP)
		}
	}

	// Direct peers remain the safe fallback for unproxied and untrusted requests
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	return rateLimitKey(host)
}

// requestFromTrustedProxy keeps caller-controlled forwarding headers from bypassing the per-client budget
func requestFromTrustedProxy(r *http.Request, trustedProxies []string) bool {
	// A non-IP peer cannot match any valid proxy configuration
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remoteIP := net.ParseIP(host)
	if remoteIP == nil {
		return false
	}

	// Accept both exact addresses and networks to cover individual proxies and load-balancer subnets
	for _, trustedProxy := range trustedProxies {
		trustedIP := net.ParseIP(trustedProxy)
		if trustedIP != nil && trustedIP.Equal(remoteIP) {
			return true
		}

		_, network, err := net.ParseCIDR(trustedProxy)
		if err == nil && network.Contains(remoteIP) {
			return true
		}
	}

	return false
}

// rateLimitKey always returns a valid Francis actor ID even for synthetic requests without a remote address
func rateLimitKey(value string) string {
	// Francis reserves slashes as actor-reference separators
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "/", "_")
	if value == "" {
		return "unknown-client"
	}
	if len(value) > 256 {
		digest := sha256.Sum256([]byte(value))
		return fmt.Sprintf("oversized-client-%x", digest)
	}
	return value
}
