//go:build unit

package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/internal/config"
)

func TestMiddlewareClientIP(t *testing.T) {
	trusted := make([]netip.Prefix, 0, 2)
	for _, proxy := range []string{"10.0.0.0/8", "2001:db8::1"} {
		prefix, err := config.ParseTrustedProxy(proxy)
		require.NoError(t, err)
		trusted = append(trusted, prefix)
	}

	// Run a request through the middleware and return what the next handler saw
	serve := func(t *testing.T, remoteAddr string, headers map[string][]string) *http.Request {
		t.Helper()

		var seen *http.Request
		handler := middlewareClientIP(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = r
		}))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/public/images", nil)
		req.RemoteAddr = remoteAddr
		for name, values := range headers {
			for _, value := range values {
				req.Header.Add(name, value)
			}
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)
		require.NotNil(t, seen)
		return seen
	}

	tests := map[string]struct {
		remoteAddr string
		headers    map[string][]string
		want       string
	}{
		"a direct client": {
			remoteAddr: "198.51.100.7:1234",
			want:       "198.51.100.7:1234",
		},
		// Anyone can send these headers, so they only count from a trusted proxy
		"a client claiming another address": {
			remoteAddr: "198.51.100.7:1234",
			headers:    map[string][]string{headerXForwardedFor: {"203.0.113.5"}, headerForwarded: {"for=203.0.113.7"}, headerXRealIP: {"203.0.113.6"}},
			want:       "198.51.100.7:1234",
		},
		"the client behind a trusted proxy": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerXForwardedFor: {"203.0.113.5"}},
			want:       "203.0.113.5",
		},
		// The entries before the last untrusted one were written by the client, so they're never the answer
		"a client that prepended a made-up address": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerXForwardedFor: {"192.0.2.99, 203.0.113.5"}},
			want:       "203.0.113.5",
		},
		"a chain of trusted proxies": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerXForwardedFor: {"203.0.113.5, 10.9.9.9"}},
			want:       "203.0.113.5",
		},
		"several header lines": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerXForwardedFor: {"192.0.2.99", "203.0.113.5, 10.9.9.9"}},
			want:       "203.0.113.5",
		},
		"an entry with a port": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerXForwardedFor: {"203.0.113.5:4321"}},
			want:       "203.0.113.5",
		},
		"X-Real-IP from a trusted proxy": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerXRealIP: {"203.0.113.5"}},
			want:       "203.0.113.5",
		},
		"an IPv6 client": {
			remoteAddr: "[2001:db8::1]:5555",
			headers:    map[string][]string{headerXForwardedFor: {"2001:db8::42"}},
			want:       "[2001:db8::42]",
		},
		"only trusted proxies forwarded it": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerXForwardedFor: {"10.7.7.7, 10.9.9.9"}},
			want:       "10.7.7.7",
		},
		"a trusted proxy that names nobody": {
			remoteAddr: "10.1.2.3:5555",
			want:       "10.1.2.3:5555",
		},
		"a malformed entry ends the trusted part": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerXForwardedFor: {"203.0.113.5, not-an-ip"}},
			want:       "10.1.2.3:5555",
		},
		"Forwarded from a trusted proxy": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerForwarded: {"for=203.0.113.5;proto=https;by=10.1.2.3"}},
			want:       "203.0.113.5",
		},
		"Forwarded with a quoted IPv6 address and a port": {
			remoteAddr: "[2001:db8::1]:5555",
			headers:    map[string][]string{headerForwarded: {`for="[2001:db8::42]:4711"`}},
			want:       "[2001:db8::42]",
		},
		"Forwarded parameter names in any case": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerForwarded: {"Proto=https;For=203.0.113.5"}},
			want:       "203.0.113.5",
		},
		"a Forwarded chain with a made-up first element": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerForwarded: {"for=192.0.2.99", "for=203.0.113.5;proto=https, for=10.9.9.9"}},
			want:       "203.0.113.5",
		},
		// A quoted value can hold a comma or a semicolon, which must not start an element or a parameter of its own
		"a Forwarded value that quotes a separator": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerForwarded: {`for=203.0.113.5;ext="x, for=192.0.2.99"`}},
			want:       "203.0.113.5",
		},
		"an obfuscated Forwarded hop ends the trusted part": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerForwarded: {"for=203.0.113.5, for=_hidden"}},
			want:       "10.1.2.3:5555",
		},
		"a Forwarded element without an address ends the trusted part": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerForwarded: {"for=203.0.113.5, proto=https"}},
			want:       "10.1.2.3:5555",
		},
		// Most proxies maintain X-Forwarded-For and pass a Forwarded header through as the client sent it
		"X-Forwarded-For is read over Forwarded": {
			remoteAddr: "10.1.2.3:5555",
			headers:    map[string][]string{headerXForwardedFor: {"203.0.113.5"}, headerForwarded: {"for=192.0.2.99"}},
			want:       "203.0.113.5",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			seen := serve(t, tt.remoteAddr, tt.headers)
			assert.Equal(t, tt.want, seen.RemoteAddr)

			// Nothing after this middleware can read an address from the headers
			assert.Empty(t, seen.Header.Values(headerXForwardedFor))
			assert.Empty(t, seen.Header.Values(headerForwarded))
			assert.Empty(t, seen.Header.Values(headerXRealIP))
		})
	}
}
