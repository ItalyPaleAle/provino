package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/webhook"
)

// Test fixtures, not credentials
//
//nolint:gosec
const (
	testSecret    = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	rotatedSecret = "whsec_QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo="
)

func testMessage() Message {
	return Message{ID: "msg_1", Timestamp: time.Now(), Body: []byte(`{"type":"image.optimized"}`)}
}

// Apply an authenticator to a fresh request and return it
func apply(t *testing.T, a Authenticator, msg Message) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://app.example.com/hooks", nil)
	err := a.Apply(t.Context(), req, msg)
	require.NoError(t, err)
	return req
}

func TestHMACIsVerifiedByTheReceiverLibrary(t *testing.T) {
	a, err := New(config.ConfigOutboundAuth{Type: config.OutboundAuthHMAC, HMAC: config.ConfigOutboundHMAC{Secrets: []string{rotatedSecret, testSecret}}}, nil, time.Second)
	require.NoError(t, err)

	msg := testMessage()
	req := apply(t, a, msg)

	// A receiver that only knows either one of the secrets verifies the request, which is what makes rotation seamless
	for _, secret := range []string{testSecret, rotatedSecret} {
		v, err := webhook.NewVerifier(t.Context(), webhook.WithHMACSecrets(secret))
		require.NoError(t, err)
		err = v.Verify(t.Context(), req.Header, msg.Body)
		require.NoError(t, err)
	}
	assert.Equal(t, "msg_1", req.Header.Get(webhook.HeaderID))
}

// Encode a private key in each of the forms the configuration accepts
func keyForms(t *testing.T, private ed25519.PrivateKey) (pemKey string, whsk string, jwk string) {
	t.Helper()

	der, err := x509.MarshalPKCS8PrivateKey(private)
	require.NoError(t, err)
	public, _ := private.Public().(ed25519.PublicKey)

	pemKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	whsk = "whsk_" + base64.StdEncoding.EncodeToString(private.Seed())
	jwk = `{"kty":"OKP","crv":"Ed25519","x":"` + base64.RawURLEncoding.EncodeToString(public) + `","d":"` + base64.RawURLEncoding.EncodeToString(private.Seed()) + `"}`
	return pemKey, whsk, jwk
}

// Add a kid to a JWK
func withKid(jwk string, kid string) string {
	return jwk[:len(jwk)-1] + `,"kid":"` + kid + `"}`
}

func TestEd25519KeySet(t *testing.T) {
	activePublic, active, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	previousPublic, previous, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	// The first key is a JWK with its own kid and the second is in the Standard Webhooks form, which has none, so the set covers both ways a key gets its ID
	_, _, activeJWK := keyForms(t, active)
	_, previousWHSK, _ := keyForms(t, previous)
	keys, err := LoadKeySet(config.ConfigSigningEd25519{Keys: []string{withKid(activeJWK, "2026-10"), previousWHSK}})
	require.NoError(t, err)
	assert.Equal(t, "2026-10", keys.ActiveKeyID())

	// The published set holds both keys, the second under the ID derived from its public key
	jwks, err := keys.JWKS()
	require.NoError(t, err)
	var set webhook.JWKS
	err = json.Unmarshal(jwks, &set)
	require.NoError(t, err)
	require.Len(t, set.Keys, 2)
	assert.Equal(t, "2026-10", set.Keys[0].Kid)
	sum := sha256.Sum256(previousPublic)
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:7]), set.Keys[1].Kid)
	published, err := set.Ed25519Keys()
	require.NoError(t, err)
	assert.Equal(t, activePublic, published[0])

	// Requests are signed with the first key, so a receiver that trusts only that one verifies them
	a, err := New(config.ConfigOutboundAuth{Type: config.OutboundAuthEd25519}, keys, time.Second)
	require.NoError(t, err)
	msg := testMessage()
	req := apply(t, a, msg)

	v, err := webhook.NewVerifier(t.Context(), webhook.WithEd25519PublicKeys(activePublic))
	require.NoError(t, err)
	err = v.Verify(t.Context(), req.Header, msg.Body)
	require.NoError(t, err)
}

func TestEd25519KeyIDs(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pemKey, whsk, jwk := keyForms(t, private)

	// A key without a kid gets the same ID whichever form it's written in, so converting it doesn't change what receivers see
	ids := make([]string, 0, 3)
	for _, value := range []string{pemKey, whsk, jwk} {
		keys, err := LoadKeySet(config.ConfigSigningEd25519{Keys: []string{value}})
		require.NoError(t, err)
		ids = append(ids, keys.ActiveKeyID())
	}
	assert.Len(t, ids[0], 10)
	assert.Equal(t, []string{ids[0], ids[0], ids[0]}, ids)

	// The same key listed twice, even in two forms, would be published twice under one ID
	_, err = LoadKeySet(config.ConfigSigningEd25519{Keys: []string{pemKey, whsk}})
	require.ErrorContains(t, err, "same ID")

	// Two different keys can't share a kid either
	_, other, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, _, otherJWK := keyForms(t, other)
	_, err = LoadKeySet(config.ConfigSigningEd25519{Keys: []string{withKid(jwk, "k1"), withKid(otherJWK, "k1")}})
	require.ErrorContains(t, err, "same ID")
}

func TestLoadKeySetWithoutKeys(t *testing.T) {
	keys, err := LoadKeySet(config.ConfigSigningEd25519{})
	require.NoError(t, err)
	assert.Nil(t, keys)

	_, err = New(config.ConfigOutboundAuth{Type: config.OutboundAuthEd25519}, nil, time.Second)
	require.Error(t, err)
}

// A fake token endpoint that records the form of every request
type tokenServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests int
	lastForm url.Values
	lastAuth string
}

func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()

	ts := &tokenServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := r.ParseForm()
		assert.NoError(t, err)

		ts.mu.Lock()
		ts.requests++
		ts.lastForm = r.PostForm
		ts.lastAuth = r.Header.Get("Authorization")
		ts.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token-1", "token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestOAuth2WithAClientSecret(t *testing.T) {
	ts := newTokenServer(t)
	a, err := New(config.ConfigOutboundAuth{Type: config.OutboundAuthOAuth2, OAuth2: config.ConfigOutboundOAuth2{
		TokenURL:       ts.URL,
		ClientID:       "provino",
		ClientSecret:   "s3cret",
		Scopes:         []string{"commit:images"},
		EndpointParams: map[string]string{"audience": "https://internal.example.com"},
	}}, nil, 5*time.Second)
	require.NoError(t, err)

	req := apply(t, a, testMessage())
	assert.Equal(t, "Bearer token-1", req.Header.Get("Authorization"))
	assert.Equal(t, "msg_1", req.Header.Get(webhook.HeaderID))
	assert.Empty(t, req.Header.Get(webhook.HeaderSignature))

	// The token is cached, so a second request doesn't cost another token request
	apply(t, a, testMessage())
	ts.mu.Lock()
	defer ts.mu.Unlock()
	assert.Equal(t, 1, ts.requests)
	assert.Equal(t, "client_credentials", ts.lastForm.Get("grant_type"))
	assert.Equal(t, "commit:images", ts.lastForm.Get("scope"))
	assert.Equal(t, "https://internal.example.com", ts.lastForm.Get("audience"))
}

func TestOAuth2WithAClientAssertion(t *testing.T) {
	ts := newTokenServer(t)
	tokenPath := filepath.Join(t.TempDir(), "token")
	err := os.WriteFile(tokenPath, []byte("federated-assertion\n"), 0o600)
	require.NoError(t, err)

	a, err := New(config.ConfigOutboundAuth{Type: config.OutboundAuthOAuth2, OAuth2: config.ConfigOutboundOAuth2{
		TokenURL:        ts.URL,
		ClientID:        "provino",
		ClientAssertion: "KubernetesServiceAccountToken=" + tokenPath,
		Scopes:          []string{"api://app/.default"},
	}}, nil, 5*time.Second)
	require.NoError(t, err)

	req := apply(t, a, testMessage())
	assert.Equal(t, "Bearer token-1", req.Header.Get("Authorization"))

	// The assertion replaces the secret: it travels in the form, and no client secret is sent in any form
	ts.mu.Lock()
	defer ts.mu.Unlock()
	assert.Equal(t, "urn:ietf:params:oauth:client-assertion-type:jwt-bearer", ts.lastForm.Get("client_assertion_type"))
	assert.Equal(t, "federated-assertion", ts.lastForm.Get("client_assertion"))
	assert.Equal(t, "provino", ts.lastForm.Get("client_id"))
	assert.Empty(t, ts.lastForm.Get("client_secret"))
	assert.Empty(t, ts.lastAuth)
}

func TestOAuth2TokenFailureFailsTheRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	a, err := New(config.ConfigOutboundAuth{Type: config.OutboundAuthOAuth2, OAuth2: config.ConfigOutboundOAuth2{
		TokenURL: server.URL, ClientID: "provino", ClientSecret: "wrong",
	}}, nil, 5*time.Second)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://app.example.com/hooks", nil)
	err = a.Apply(t.Context(), req, testMessage())
	require.Error(t, err)
	assert.Empty(t, req.Header.Get("Authorization"))
	assert.Positive(t, calls.Load())
}

func TestNoneSendsOnlyTheMessageHeaders(t *testing.T) {
	a, err := New(config.ConfigOutboundAuth{Type: config.OutboundAuthNone, AllowUnauthenticated: true}, nil, time.Second)
	require.NoError(t, err)

	req := apply(t, a, testMessage())
	assert.Equal(t, "msg_1", req.Header.Get(webhook.HeaderID))
	assert.NotEmpty(t, req.Header.Get(webhook.HeaderTimestamp))
	assert.Empty(t, req.Header.Get(webhook.HeaderSignature))
	assert.Empty(t, req.Header.Get("Authorization"))
}
