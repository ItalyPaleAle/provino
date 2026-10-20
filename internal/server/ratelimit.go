package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/italypaleale/provino/internal/auth"
)

// RateLimiter is the Francis built-in rate-limit service used by the HTTP layer
type RateLimiter interface {
	Allow(ctx context.Context, key string) (allowed bool, retryAfter time.Duration, err error)
}

// middlewareRateLimit rejects excess requests before their upload bodies are read or stored
// It runs after authentication, so the budget belongs to the caller rather than to an address that a whole office can share
func (s *Server) middlewareRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every caller that passed authentication has a subject, so a request without one never went through it
		principal, ok := auth.PrincipalFromContext(r.Context())
		if !ok || principal.Subject == "" {
			errUnauthorized.WriteResponse(w, r)
			return
		}

		// Route the per-caller decision through Francis before reading the upload body
		allowed, retryAfter, err := s.rateLimiter.Allow(r.Context(), rateLimitKey(principal.Subject))
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

// rateLimitKey keys the budget on the authenticated caller
// The subject is hashed, so a key never repeats an identifier from a token in the actor store, and it's always a valid actor ID whatever characters the subject has
func rateLimitKey(subject string) string {
	digest := sha256.Sum256([]byte(subject))
	return "sub-" + hex.EncodeToString(digest[:16])
}
