package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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

func TestEd25519KeySet(t *testing.T) {
	_, active, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	retired, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	// The active key comes from a file, the retired one inline, which covers both ways of configuring a key
	activePath := filepath.Join(t.TempDir(), "active.key")
	err = os.WriteFile(activePath, []byte("whsk_"+base64.StdEncoding.EncodeToString(active.Seed())), 0o600)
	require.NoError(t, err)

	keys, err := LoadKeySet(config.ConfigSigningEd25519{
		ActiveKey: "2026-10",
		Keys: []config.ConfigSigningKey{
			{ID: "2026-10", PrivateKeyFile: activePath},
			{ID: "2026-01", PublicKey: webhook.EncodeEd25519PublicKey(retired)},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "2026-10", keys.ActiveKeyID())

	// The published set holds both keys, and a receiver that fetched it verifies requests signed with the active one
	jwks, err := keys.JWKS()
	require.NoError(t, err)
	var set webhook.JWKS
	err = json.Unmarshal(jwks, &set)
	require.NoError(t, err)
	require.Len(t, set.Keys, 2)
	published, err := set.Ed25519Keys()
	require.NoError(t, err)

	a, err := New(config.ConfigOutboundAuth{Type: config.OutboundAuthEd25519}, keys, time.Second)
	require.NoError(t, err)
	msg := testMessage()
	req := apply(t, a, msg)

	v, err := webhook.NewVerifier(t.Context(), webhook.WithEd25519PublicKeys(published...))
	require.NoError(t, err)
	err = v.Verify(t.Context(), req.Header, msg.Body)
	require.NoError(t, err)
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
