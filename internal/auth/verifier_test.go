package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAudience = "https://images.example.test"

// An issuer that publishes a key set the way an identity provider does, and signs tokens with it
type testIssuer struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
	// declaredIssuer overrides what the discovery document names, which is how a trailing slash is exercised
	declaredIssuer string
	// keyAlg is the "alg" the published key declares, or empty to leave it to the verifier to infer
	keyAlg string
	// extraKeys are published next to the RSA key
	extraKeys []map[string]string
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()

	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	issuer := &testIssuer{key: private, kid: "test-key", keyAlg: "RS256"}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   issuer.issuerClaim(),
			"jwks_uri": issuer.server.URL + "/.well-known/jwks.json",
		})
	})
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		key := map[string]string{
			"kty": "RSA", "use": "sig",
			"kid": issuer.kid,
			"n":   base64.RawURLEncoding.EncodeToString(private.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(private.E)).Bytes()),
		}
		if issuer.keyAlg != "" {
			key["alg"] = issuer.keyAlg
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": append([]map[string]string{key}, issuer.extraKeys...),
		})
	})

	issuer.server = httptest.NewServer(mux)
	t.Cleanup(issuer.server.Close)

	return issuer
}

// issuerClaim is what the discovery document declares and what tokens carry in `iss`
func (i *testIssuer) issuerClaim() string {
	if i.declaredIssuer != "" {
		return i.declaredIssuer
	}
	return i.server.URL
}

// Sign a token with RS256, the algorithm the published key declares by default
func (i *testIssuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()

	return i.signWithAlg(t, jwa.RS256(), claims)
}

func (i *testIssuer) signWithAlg(t *testing.T, alg jwa.SignatureAlgorithm, claims map[string]any) string {
	t.Helper()

	payload, err := json.Marshal(claims)
	require.NoError(t, err)

	headers := jws.NewHeaders()
	err = headers.Set(jws.KeyIDKey, i.kid)
	require.NoError(t, err)

	signed, err := jws.Sign(payload, jws.WithKey(alg, i.key, jws.WithProtectedHeaders(headers)))
	require.NoError(t, err)

	return string(signed)
}

func (i *testIssuer) claims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":   i.issuerClaim(),
		"aud":   testAudience,
		"sub":   "user-01a061a9",
		"iat":   now.Unix(),
		"exp":   now.Add(15 * time.Minute).Unix(),
		"scope": "ingest openid",
	}
}

func newTestVerifier(t *testing.T, issuer *testIssuer) *Verifier {
	t.Helper()

	return newTestVerifierWith(t, issuer, VerifierOptions{})
}

func newTestVerifierWith(t *testing.T, issuer *testIssuer, opts VerifierOptions) *Verifier {
	t.Helper()

	opts.Issuer = issuer.server.URL
	opts.Audience = testAudience
	v, err := NewVerifier(t.Context(), issuer.server.Client(), opts)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := v.Close(context.Background())
		require.NoError(t, err)
	})

	return v
}

func TestVerifyAcceptsAToken(t *testing.T) {
	issuer := newTestIssuer(t)
	v := newTestVerifier(t, issuer)

	principal, err := v.Verify(t.Context(), issuer.sign(t, issuer.claims()), time.Now())
	require.NoError(t, err)
	assert.Equal(t, "user-01a061a9", principal.Subject)
	assert.Equal(t, []string{"ingest", "openid"}, principal.Scopes)
	assert.Equal(t, "jwt", principal.Method)
}

// Providers commonly declare an issuer with a trailing slash and put that exact value in every token
// The configured issuer is normalized for the network boundary, so what tokens are checked against has to come from the discovery document instead
func TestVerifyAcceptsAnIssuerDeclaredWithATrailingSlash(t *testing.T) {
	issuer := newTestIssuer(t)
	issuer.declaredIssuer = issuer.server.URL + "/"
	v := newTestVerifier(t, issuer)

	principal, err := v.Verify(t.Context(), issuer.sign(t, issuer.claims()), time.Now())

	require.NoError(t, err)
	assert.Equal(t, "user-01a061a9", principal.Subject)
}

func TestVerifyRejects(t *testing.T) {
	issuer := newTestIssuer(t)
	v := newTestVerifier(t, issuer)
	now := time.Now()

	t.Run("a signature from another key", func(t *testing.T) {
		other := newTestIssuer(t)
		other.kid = issuer.kid
		other.declaredIssuer = issuer.server.URL
		_, err := v.Verify(t.Context(), other.sign(t, issuer.claims()), now)
		require.ErrorIs(t, err, ErrUnauthorized)
	})

	// The algorithm comes from this service rather than from the token, so a token that declares another one is refused before anything in it is read
	t.Run("an algorithm the service does not accept", func(t *testing.T) {
		claims := issuer.claims()
		payload, err := json.Marshal(claims)
		require.NoError(t, err)
		header, err := json.Marshal(map[string]string{"alg": "none", "kid": issuer.kid})
		require.NoError(t, err)
		token := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."

		_, err = v.Verify(t.Context(), token, now)
		require.ErrorIs(t, err, ErrUnauthorized)
	})

	// PS256 is a valid RSA signature over the same key, but the key declares RS256 and the key set is what decides
	t.Run("an algorithm the key does not declare", func(t *testing.T) {
		_, err := v.Verify(t.Context(), issuer.signWithAlg(t, jwa.PS256(), issuer.claims()), now)
		require.ErrorIs(t, err, ErrUnauthorized)
	})

	// A token minted for another API must not be spendable here, however valid its signature is
	t.Run("another audience", func(t *testing.T) {
		claims := issuer.claims()
		claims["aud"] = "somewhere-else"
		_, err := v.Verify(t.Context(), issuer.sign(t, claims), now)
		require.ErrorIs(t, err, ErrUnauthorized)
	})

	t.Run("another issuer", func(t *testing.T) {
		claims := issuer.claims()
		claims["iss"] = "https://elsewhere.example"
		_, err := v.Verify(t.Context(), issuer.sign(t, claims), now)
		require.ErrorIs(t, err, ErrUnauthorized)
	})

	t.Run("an expired token", func(t *testing.T) {
		claims := issuer.claims()
		claims["exp"] = now.Add(-time.Hour).Unix()
		_, err := v.Verify(t.Context(), issuer.sign(t, claims), now)
		require.ErrorIs(t, err, ErrUnauthorized)
	})

	// Without a subject there is nobody to authorize, so the token authenticates nothing this service can act on
	t.Run("a token naming no subject", func(t *testing.T) {
		claims := issuer.claims()
		delete(claims, "sub")
		_, err := v.Verify(t.Context(), issuer.sign(t, claims), now)
		require.ErrorIs(t, err, ErrUnauthorized)
	})

	t.Run("something that is not a compact JWS", func(t *testing.T) {
		for _, token := range []string{"", "a", "a.b", "....", "a.b.c"} {
			_, err := v.Verify(t.Context(), token, now)
			require.ErrorIs(t, err, ErrUnauthorized, token)
		}
	})
}

// An issuer whose own metadata names someone else is not one whose keys can be trusted for it
func TestNewVerifierRejectsMismatchedMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   "https://elsewhere.example",
			"jwks_uri": "https://elsewhere.example/.well-known/jwks.json",
		})
	}))
	t.Cleanup(server.Close)

	_, err := NewVerifier(t.Context(), server.Client(), VerifierOptions{Issuer: server.URL, Audience: testAudience})
	require.ErrorContains(t, err, "different issuer")
}

// Discovery cannot turn the configured issuer origin into an open network fetcher
func TestNewVerifierRejectsCrossOriginKeySet(t *testing.T) {
	var targetFetched atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetFetched.Store(true)
	}))
	t.Cleanup(target.Close)

	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   issuer.URL,
			"jwks_uri": target.URL,
		})
	}))
	t.Cleanup(issuer.Close)

	_, err := NewVerifier(t.Context(), issuer.Client(), VerifierOptions{Issuer: issuer.URL, Audience: testAudience})

	require.ErrorContains(t, err, "must use the issuer's origin")
	assert.False(t, targetFetched.Load())
}

// Redirect policy applies to background refreshes as well as the initial key load
func TestNewVerifierRejectsCrossOriginKeySetRedirect(t *testing.T) {
	var targetFetched atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetFetched.Store(true)
	}))
	t.Cleanup(target.Close)

	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":   issuer.URL,
				"jwks_uri": issuer.URL + "/.well-known/jwks.json",
			})
		case "/.well-known/jwks.json":
			http.Redirect(w, r, target.URL, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(issuer.Close)

	_, err := NewVerifier(t.Context(), issuer.Client(), VerifierOptions{Issuer: issuer.URL, Audience: testAudience})

	require.ErrorContains(t, err, "redirects may not leave the issuer's origin")
	assert.False(t, targetFetched.Load())
}

func TestParseIssuerResourceEnforcesTheIssuerOrigin(t *testing.T) {
	issuer, err := parseIssuer("https://idp.example")
	require.NoError(t, err)

	tests := []struct {
		name      string
		resource  string
		wantError bool
	}{
		{name: "issuer path", resource: "https://idp.example/.well-known/jwks.json"},
		{name: "explicit default port", resource: "https://idp.example:443/.well-known/jwks.json"},
		{name: "another host", resource: "https://metadata.internal/latest", wantError: true},
		{name: "another port", resource: "https://idp.example:8443/.well-known/jwks.json", wantError: true},
		{name: "credentials", resource: "https://idp.example@metadata.internal/latest", wantError: true},
		{name: "scheme downgrade", resource: "http://idp.example/.well-known/jwks.json", wantError: true},
		{name: "relative URL", resource: "/.well-known/jwks.json", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseIssuerResource(test.resource, issuer)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// The service does not start when the issuer cannot be reached, rather than accepting nothing one request at a time
func TestNewVerifierFailsWhenTheIssuerIsUnreachable(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()

	_, err := NewVerifier(t.Context(), server.Client(), VerifierOptions{Issuer: server.URL, Audience: testAudience})
	require.Error(t, err)
}

// A key that doesn't declare an algorithm lets the verifier infer it from the key type, so the key set rather than this service decides which RSA algorithms work
func TestVerifyInfersTheAlgorithmFromTheKey(t *testing.T) {
	issuer := newTestIssuer(t)
	issuer.keyAlg = ""
	v := newTestVerifier(t, issuer)

	for _, alg := range []jwa.SignatureAlgorithm{jwa.RS256(), jwa.PS256(), jwa.RS512()} {
		_, err := v.Verify(t.Context(), issuer.signWithAlg(t, alg, issuer.claims()), time.Now())
		require.NoError(t, err, alg.String())
	}
}

// A symmetric key in the key set can never verify a token, even one whose header asks for HMAC
func TestVerifyRefusesSymmetricSignatures(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	issuer := newTestIssuer(t)
	issuer.extraKeys = []map[string]string{{"kty": "oct", "kid": "shared", "alg": "HS256", "k": base64.RawURLEncoding.EncodeToString(secret)}}
	v := newTestVerifier(t, issuer)

	payload, err := json.Marshal(issuer.claims())
	require.NoError(t, err)
	headers := jws.NewHeaders()
	err = headers.Set(jws.KeyIDKey, "shared")
	require.NoError(t, err)
	signed, err := jws.Sign(payload, jws.WithKey(jwa.HS256(), secret, jws.WithProtectedHeaders(headers)))
	require.NoError(t, err)

	_, err = v.Verify(t.Context(), string(signed), time.Now())
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestVerifyReadsConfiguredClaims(t *testing.T) {
	issuer := newTestIssuer(t)
	v := newTestVerifierWith(t, issuer, VerifierOptions{SubjectClaim: "oid", ScopeClaim: "roles"})

	claims := issuer.claims()
	claims["oid"] = "00000000-0000-0000-0000-000000000001"
	claims["roles"] = []string{"server", "ingest"}
	principal, err := v.Verify(t.Context(), issuer.sign(t, claims), time.Now())
	require.NoError(t, err)
	assert.Equal(t, "00000000-0000-0000-0000-000000000001", principal.Subject)
	assert.Equal(t, []string{"server", "ingest"}, principal.Scopes)

	// The standard subject is not a fallback: a token without the configured claim names nobody
	delete(claims, "oid")
	_, err = v.Verify(t.Context(), issuer.sign(t, claims), time.Now())
	require.ErrorIs(t, err, ErrUnauthorized)
}

// Providers that host several tenants on one origin put the tenant in the issuer's path, and discovery happens under it
func TestNewVerifierAcceptsAnIssuerWithAPath(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /realms/demo/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": server.URL + "/realms/demo", "jwks_uri": server.URL + "/realms/demo/certs"})
	})
	mux.HandleFunc("GET /realms/demo/certs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k",
			"n": base64.RawURLEncoding.EncodeToString(private.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(private.E)).Bytes()),
		}}})
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)

	v, err := NewVerifier(t.Context(), server.Client(), VerifierOptions{Issuer: server.URL + "/realms/demo", Audience: testAudience})
	require.NoError(t, err)
	t.Cleanup(func() {
		err := v.Close(context.Background())
		require.NoError(t, err)
	})
	assert.Equal(t, server.URL+"/realms/demo", v.Issuer())
}

// An operator that names the key set skips discovery entirely, for providers that don't publish it
func TestNewVerifierWithAnExplicitKeySet(t *testing.T) {
	issuer := newTestIssuer(t)
	v := newTestVerifierWith(t, issuer, VerifierOptions{JWKSURL: issuer.server.URL + "/.well-known/jwks.json"})

	principal, err := v.Verify(t.Context(), issuer.sign(t, issuer.claims()), time.Now())
	require.NoError(t, err)
	assert.Equal(t, "user-01a061a9", principal.Subject)
}
