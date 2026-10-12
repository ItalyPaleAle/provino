//go:build unit

package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/internal/auth"
)

type stubRateLimiter struct {
	allowed    bool
	retryAfter time.Duration
	err        error
	keys       []string
}

func (s *stubRateLimiter) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	s.keys = append(s.keys, key)
	return s.allowed, s.retryAfter, s.err
}

func TestMiddlewareRateLimit(t *testing.T) {
	t.Run("allows the request", func(t *testing.T) {
		limiter := &stubRateLimiter{allowed: true}
		s := &Server{rateLimiter: limiter}
		handler := s.middlewareRateLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/public/ingest", nil)
		req.RemoteAddr = "198.51.100.7:1234"
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, []string{"ip-198.51.100.7"}, limiter.keys)
	})

	// The budget belongs to the caller, so two callers behind one address don't share it, and the key never repeats the subject itself
	t.Run("keys the budget on the caller's subject", func(t *testing.T) {
		limiter := &stubRateLimiter{allowed: true}
		s := &Server{rateLimiter: limiter}
		handler := s.middlewareRateLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

		for _, subject := range []string{"user-1", "user-2", "user-1"} {
			req := httptest.NewRequestWithContext(auth.WithPrincipal(t.Context(), auth.Principal{Subject: subject}), http.MethodPost, "/v1/public/ingest", nil)
			req.RemoteAddr = "198.51.100.7:1234"
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}

		require.Len(t, limiter.keys, 3)
		assert.NotEqual(t, limiter.keys[0], limiter.keys[1])
		assert.Equal(t, limiter.keys[0], limiter.keys[2])
		assert.True(t, strings.HasPrefix(limiter.keys[0], "sub-"))
		assert.NotContains(t, limiter.keys[0], "user-1")
	})

	t.Run("rejects the request with retry-after", func(t *testing.T) {
		limiter := &stubRateLimiter{retryAfter: 1500 * time.Millisecond}
		s := &Server{rateLimiter: limiter}
		handler := s.middlewareRateLimit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("rate-limited request reached the handler")
		}))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/public/ingest", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusTooManyRequests, rec.Code)
		assert.Equal(t, "2", rec.Header().Get("Retry-After"))
		assert.Contains(t, rec.Body.String(), `"code":"rate_limited"`)
	})

	t.Run("fails closed when Francis is unavailable", func(t *testing.T) {
		limiter := &stubRateLimiter{err: errors.New("actor host unavailable")}
		s := &Server{rateLimiter: limiter}
		handler := s.middlewareRateLimit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("request reached the handler after a rate-limit error")
		}))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/public/ingest", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

func TestClientIP(t *testing.T) {
	t.Run("ignores forwarded headers from an untrusted peer", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/public/ingest", nil)
		req.RemoteAddr = "192.0.2.10:1234"
		req.Header.Set("X-Forwarded-For", "198.51.100.7")

		assert.Equal(t, "192.0.2.10", clientIP(req, nil))
	})

	t.Run("uses the original client behind a trusted proxy", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/public/ingest", nil)
		req.RemoteAddr = "192.0.2.10:1234"
		req.Header.Set("X-Forwarded-For", "198.51.100.7, 192.0.2.10")

		assert.Equal(t, "198.51.100.7", clientIP(req, []string{"192.0.2.0/24"}))
	})

	t.Run("always returns a Francis actor ID", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/public/ingest", nil)
		req.RemoteAddr = ""

		key := clientIP(req, nil)
		require.NotEmpty(t, key)
		assert.NotContains(t, key, "/")
	})
}
