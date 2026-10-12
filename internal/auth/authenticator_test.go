package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/internal/config"
)

const (
	testKey      = "pvk_0123456789abcdefghijklmnopqrstuv"
	otherTestKey = "pvk_vutsrqponmlkjihgfedcba9876543210"
)

func requestWithToken(token string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestAPIKeys(t *testing.T) {
	digest := sha256.Sum256([]byte(otherTestKey))
	keys := NewAPIKeys([]config.ConfigAPIKey{
		{ID: "plain", Key: testKey, Subject: "backend", Scopes: []string{"server"}},
		{ID: "hashed", SHA256: hex.EncodeToString(digest[:]), Subject: "other"},
	})

	principal, err := keys.Verify(testKey)
	require.NoError(t, err)
	assert.Equal(t, Principal{Subject: "backend", Scopes: []string{"server"}, Method: "apikey"}, principal)

	// A key configured by its digest authenticates the same way, without the key being in the configuration
	principal, err = keys.Verify(otherTestKey)
	require.NoError(t, err)
	assert.Equal(t, "other", principal.Subject)

	_, err = keys.Verify("pvk_unknown")
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestSetAuthenticate(t *testing.T) {
	t.Run("routes API keys to the key authenticator", func(t *testing.T) {
		s, err := NewSet(t.Context(), config.ConfigAuth{APIKeys: []config.ConfigAPIKey{{ID: "k", Key: testKey, Subject: "k"}}}, "")
		require.NoError(t, err)

		principal, err := s.Authenticate(t.Context(), requestWithToken(testKey))
		require.NoError(t, err)
		assert.Equal(t, "k", principal.Subject)

		// Without a JWT verifier, anything that isn't an API key is refused rather than parsed
		_, err = s.Authenticate(t.Context(), requestWithToken("eyJhbGciOiJSUzI1NiJ9.e30.sig"))
		require.ErrorIs(t, err, ErrUnauthorized)

		_, err = s.Authenticate(t.Context(), requestWithToken(""))
		require.ErrorIs(t, err, ErrUnauthorized)
	})

	t.Run("routes JWTs to the verifier", func(t *testing.T) {
		issuer := newTestIssuer(t)
		s, err := NewSet(t.Context(), config.ConfigAuth{JWT: config.ConfigAuthJWT{Issuer: issuer.server.URL, Audience: testAudience}}, "")
		require.NoError(t, err)
		t.Cleanup(func() {
			err := s.Close(context.Background())
			require.NoError(t, err)
		})

		principal, err := s.Authenticate(t.Context(), requestWithToken(issuer.sign(t, issuer.claims())))
		require.NoError(t, err)
		assert.Equal(t, "user-01a061a9", principal.Subject)

		// Without API keys configured, a key is refused rather than handed to the verifier
		_, err = s.Authenticate(t.Context(), requestWithToken(testKey))
		require.ErrorIs(t, err, ErrUnauthorized)
	})

	t.Run("none accepts every request as the configured principal", func(t *testing.T) {
		s, err := NewSet(t.Context(), config.ConfigAuth{None: config.ConfigAuthNone{Enabled: true, Subject: "dev", Scopes: []string{"server"}}}, "")
		require.NoError(t, err)

		principal, err := s.Authenticate(t.Context(), requestWithToken(""))
		require.NoError(t, err)
		assert.Equal(t, Principal{Subject: "dev", Scopes: []string{"server"}, Method: "none"}, principal)
	})
}

func TestMiddleware(t *testing.T) {
	keys, err := NewSet(t.Context(), config.ConfigAuth{APIKeys: []config.ConfigAPIKey{
		{ID: "server", Key: testKey, Subject: "server", Scopes: []string{"server"}},
		{ID: "browser", Key: otherTestKey, Subject: "browser", Scopes: []string{"ingest"}},
	}}, "")
	require.NoError(t, err)

	var seen Principal
	handler := Middleware(keys, "server",
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
	)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := map[string]struct {
		token string
		want  int
	}{
		"a caller with the scope":    {token: testKey, want: http.StatusNoContent},
		"a caller without the scope": {token: otherTestKey, want: http.StatusForbidden},
		"an unknown key":             {token: "pvk_nope", want: http.StatusUnauthorized},
		"no credentials":             {want: http.StatusUnauthorized},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, requestWithToken(tt.token))
			assert.Equal(t, tt.want, rec.Code)
		})
	}

	assert.Equal(t, "server", seen.Subject)
}

func TestPrincipalHasScope(t *testing.T) {
	p := Principal{Scopes: []string{"ingest"}}
	assert.True(t, p.HasScope("ingest"))
	assert.False(t, p.HasScope("server"))
	// A route that requires no scope accepts any authenticated caller
	assert.True(t, p.HasScope(""))
}
