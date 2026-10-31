package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/italypaleale/provino/internal/auth"
	"github.com/italypaleale/provino/manifest"
	"github.com/italypaleale/provino/webhook"
)

// The audience of the users' tokens in the tests, which no request reaches
const testProvinoURL = "http://provino.test"

// testServer is the app, served on a local port
type testServer struct {
	*httptest.Server

	app     *app
	dataDir string
	// Client that doesn't follow redirects, so the sign-in's redirects can be inspected
	noRedirect *http.Client
}

// Start the app with a data folder of its own
func startTestServer(t *testing.T) *testServer {
	t.Helper()

	// The app's public URL is its issuer, so the server has to listen before the app is created
	srv := httptest.NewUnstartedServer(nil)
	dataDir := t.TempDir()
	a, err := newApp(t.Context(), slog.New(slog.DiscardHandler), options{
		PublicURL:  "http://" + srv.Listener.Addr().String(),
		ProvinoURL: testProvinoURL,
		DataDir:    dataDir,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	srv.Config.Handler = a.routes()
	srv.Start()
	t.Cleanup(srv.Close)

	noRedirect := srv.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &testServer{Server: srv, app: a, dataDir: dataDir, noRedirect: noRedirect}
}

// request is one call the tests make to the app
type request struct {
	method      string
	path        string
	body        string
	contentType string
	token       string
}

// response is what the app answered a request with
type response struct {
	StatusCode int
	Header     http.Header
}

// Send a request with a client, returning the response and its body, which is already read and closed
func (s *testServer) call(t *testing.T, c *http.Client, r request) (response, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), r.method, s.URL+r.path, strings.NewReader(r.body))
	require.NoError(t, err)
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}

	res, err := c.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return response{StatusCode: res.StatusCode, Header: res.Header}, body
}

// Post a form without following the redirect it answers with
func (s *testServer) postForm(t *testing.T, path string, form url.Values) (response, []byte) {
	t.Helper()

	return s.call(t, s.noRedirect, request{method: http.MethodPost, path: path, body: form.Encode(), contentType: "application/x-www-form-urlencoded"})
}

// Read a JSON list from the app's API as a signed-in user
func (s *testServer) userGet(t *testing.T, token string, path string, dst any) {
	t.Helper()

	res, body := s.call(t, s.Client(), request{method: http.MethodGet, path: path, token: token})
	require.Equal(t, http.StatusOK, res.StatusCode, string(body))
	err := json.Unmarshal(body, dst)
	require.NoError(t, err)
}

// Sign in as an account the way the web UI does, with the authorization code grant, and return the token
func (s *testServer) signIn(t *testing.T, accountID string) string {
	t.Helper()

	params := url.Values{
		"response_type": {"code"},
		"client_id":     {webClientID},
		"redirect_uri":  {s.URL + "/callback"},
		"state":         {"xyz"},
	}

	// The sign-in page lists the accounts
	res, page := s.call(t, s.noRedirect, request{method: http.MethodGet, path: "/oauth/authorize?" + params.Encode()})
	require.Equal(t, http.StatusOK, res.StatusCode, string(page))
	assert.Contains(t, string(page), "Alice")

	// Picking an account redirects back to the web UI with a code and the state
	params.Set("account", accountID)
	res, _ = s.postForm(t, "/oauth/authorize", params)
	require.Equal(t, http.StatusSeeOther, res.StatusCode)
	location, err := url.Parse(res.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "/callback", location.Path)
	assert.Equal(t, "xyz", location.Query().Get("state"))

	exchange := url.Values{
		"grant_type":   {"authorization_code"},
		"client_id":    {webClientID},
		"code":         {location.Query().Get("code")},
		"redirect_uri": {s.URL + "/callback"},
	}
	body := s.token(t, exchange, http.StatusOK)

	// A code can be exchanged once only
	again := s.token(t, exchange, http.StatusBadRequest)
	assert.Equal(t, "invalid_grant", again["error"])

	token, ok := body["access_token"].(string)
	require.True(t, ok)
	return token
}

// Post to the token endpoint, check the status, and return the answer
func (s *testServer) token(t *testing.T, form url.Values, status int) map[string]any {
	t.Helper()

	res, raw := s.postForm(t, "/oauth/token", form)
	var body map[string]any
	err := json.Unmarshal(raw, &body)
	require.NoError(t, err)
	require.Equal(t, status, res.StatusCode, body)
	return body
}

// A client that calls the app the way provino does, with a client-credentials token
func (s *testServer) provinoClient(t *testing.T) *http.Client {
	t.Helper()

	cfg := clientcredentials.Config{ClientID: provinoClientID, ClientSecret: "any secret works", TokenURL: s.URL + "/oauth/token"}
	return cfg.Client(t.Context())
}

// The tokens users sign in for are the ones provino accepts, which is checked with provino's own verifier
func TestUserTokensVerifyAtProvino(t *testing.T) {
	s := startTestServer(t)
	token := s.signIn(t, "alice")

	verifier, err := auth.NewVerifier(t.Context(), s.Client(), auth.VerifierOptions{Issuer: s.URL, Audience: testProvinoURL})
	require.NoError(t, err)
	t.Cleanup(func() { _ = verifier.Close(t.Context()) })

	principal, err := verifier.Verify(t.Context(), token, time.Now())
	require.NoError(t, err)
	assert.Equal(t, "alice", principal.Subject)
	assert.Contains(t, principal.Scopes, ingestScope)

	// The signing key is in the database, so a restarted app keeps signing tokens provino already trusts
	reopened, err := openDB(t.Context(), filepath.Join(s.dataDir, "app", "example.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	key, err := loadSigningKey(t.Context(), reopened)
	require.NoError(t, err)
	kid, _ := key.KeyID()
	ownKid, _ := s.app.oauth.key.KeyID()
	assert.Equal(t, ownKid, kid)

	t.Run("a code is exchanged only with the redirect URI it was sent to", func(t *testing.T) {
		res, _ := s.postForm(t, "/oauth/authorize", url.Values{
			"response_type": {"code"},
			"client_id":     {webClientID},
			"redirect_uri":  {s.URL + "/callback"},
			"account":       {"bob"},
		})
		location, err := url.Parse(res.Header.Get("Location"))
		require.NoError(t, err)

		body := s.token(t, url.Values{
			"grant_type":   {"authorization_code"},
			"client_id":    {webClientID},
			"code":         {location.Query().Get("code")},
			"redirect_uri": {s.URL + "/elsewhere"},
		}, http.StatusBadRequest)
		assert.Equal(t, "invalid_grant", body["error"])
	})

	t.Run("only the web UI's callback can receive a code", func(t *testing.T) {
		res, _ := s.call(t, s.noRedirect, request{method: http.MethodGet, path: "/oauth/authorize?" + url.Values{
			"response_type": {"code"},
			"client_id":     {webClientID},
			"redirect_uri":  {"https://elsewhere.example.com/callback"},
		}.Encode()})
		assert.Equal(t, http.StatusBadRequest, res.StatusCode)
	})
}

// The authorizer decides from the hardcoded rules, only answers provino, and records every request it answers
func TestAuthorizer(t *testing.T) {
	s := startTestServer(t)
	client := s.provinoClient(t)

	authorize := func(t *testing.T, c *http.Client, subject string, uploadCtx string) (response, webhook.AuthorizeResponse) {
		t.Helper()
		res, body := s.call(t, c, request{
			method:      http.MethodPost,
			path:        "/api/provino/authorize",
			body:        `{"version":"provino/v1","subject":"` + subject + `","context":` + uploadCtx + `}`,
			contentType: "application/json",
		})
		var decision webhook.AuthorizeResponse
		if res.StatusCode == http.StatusOK {
			err := json.Unmarshal(body, &decision)
			require.NoError(t, err)
		}
		return res, decision
	}

	res, decision := authorize(t, client, "alice", `{"profile":"photo"}`)
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, "alice", decision.Namespace)
	assert.Equal(t, int64(20<<20), decision.MaxBytes)
	assert.Empty(t, decision.AllowedFormats)
	assert.Len(t, decision.Variants, 3)
	require.NotNil(t, decision.Notify)
	assert.Equal(t, notifyEndpoint, decision.Notify.Endpoint)
	assert.JSONEq(t, `{"owner":"alice","profile":"photo"}`, string(decision.Notify.Context))

	res, decision = authorize(t, client, "bob", `{"profile":"avatar"}`)
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, []string{"jpg", "png"}, decision.AllowedFormats)
	assert.Equal(t, int64(2<<20), decision.MaxBytes)

	// A profile that isn't hardcoded, or an account the app doesn't know, is refused
	res, _ = authorize(t, client, "bob", `{"profile":"poster"}`)
	assert.Equal(t, http.StatusForbidden, res.StatusCode)
	res, _ = authorize(t, client, "mallory", `{"profile":"photo"}`)
	assert.Equal(t, http.StatusForbidden, res.StatusCode)

	// Without provino's token, or with a user's, nothing is decided
	res, _ = authorize(t, s.Client(), "alice", `{"profile":"photo"}`)
	assert.Equal(t, http.StatusUnauthorized, res.StatusCode)
	alice := s.signIn(t, "alice")
	res, _ = s.call(t, s.Client(), request{method: http.MethodPost, path: "/api/provino/authorize", body: `{"version":"provino/v1","subject":"alice"}`, token: alice})
	assert.Equal(t, http.StatusUnauthorized, res.StatusCode)

	// Each account sees the requests made for its own uploads, with the app's answers and without the token's secret part
	var aliceRequests []authorizationRecord
	s.userGet(t, alice, "/api/authorizations", &aliceRequests)
	require.Len(t, aliceRequests, 1)
	assert.Equal(t, http.StatusOK, aliceRequests[0].Status)
	assert.JSONEq(t, `{"version":"provino/v1","subject":"alice","context":{"profile":"photo"}}`, string(aliceRequests[0].Request))
	assert.Contains(t, string(aliceRequests[0].Response), `"namespace":"alice"`)
	assert.True(t, strings.HasSuffix(aliceRequests[0].RequestHeaders["Authorization"], "…"))
	assert.Less(t, len(aliceRequests[0].RequestHeaders["Authorization"]), 30)

	var bobRequests []authorizationRecord
	s.userGet(t, s.signIn(t, "bob"), "/api/authorizations", &bobRequests)
	require.Len(t, bobRequests, 2)
	assert.Equal(t, http.StatusForbidden, bobRequests[0].Status)
	assert.Equal(t, http.StatusOK, bobRequests[1].Status)
}

// Webhook deliveries are recorded as they arrived, and applied once, in the order the events happened, for the account that owns the image
func TestEventsAreRecordedOnceAndInOrder(t *testing.T) {
	s := startTestServer(t)
	client := s.provinoClient(t)

	deliver := func(t *testing.T, id string, e webhook.Event) {
		t.Helper()
		body, err := json.Marshal(e)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL+"/api/provino/events", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webhook.HeaderID, id)
		req.Header.Set(webhook.HeaderTimestamp, "1")
		res, err := client.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		require.Equal(t, http.StatusNoContent, res.StatusCode)
	}

	now := time.Now().UTC().Truncate(time.Second)
	data := webhook.EventData{
		Namespace: "alice",
		ImageID:   "0b8cb0f7-2a1a-4f0f-9a9e-2b3a3f2f5f2a",
		JobID:     "019a7c3e-5b2d-7f41-9c8a-2d4e6f8a0b1c",
		Manifest:  &manifest.Manifest{Version: manifest.Version, Status: manifest.StatusCompleted},
		Context:   json.RawMessage(`{"owner":"alice","profile":"photo"}`),
	}
	deliver(t, "job-1.image.optimized", webhook.Event{Type: webhook.EventImageOptimized, Timestamp: now, Data: data})
	deliver(t, "job-1.image.optimized", webhook.Event{Type: webhook.EventImageOptimized, Timestamp: now, Data: data})
	older := data
	older.Error = "an older job's failure"
	deliver(t, "job-0.image.failed", webhook.Event{Type: webhook.EventImageFailed, Timestamp: now.Add(-time.Minute), Data: older})

	alice := s.signIn(t, "alice")
	var images []imageRecord
	s.userGet(t, alice, "/api/images", &images)
	require.Len(t, images, 1)
	assert.Equal(t, webhook.EventImageOptimized, images[0].Status)
	assert.Equal(t, "photo", images[0].Profile)
	assert.Empty(t, images[0].Error)
	require.NotNil(t, images[0].Manifest)

	// Every delivery is listed with what the app did with it, its headers, and the event as provino sent it
	var deliveries []deliveryRecord
	s.userGet(t, alice, "/api/deliveries", &deliveries)
	require.Len(t, deliveries, 3)
	assert.Equal(t, []string{outcomeStale, outcomeRepeat, outcomeApplied}, []string{deliveries[0].Outcome, deliveries[1].Outcome, deliveries[2].Outcome})
	assert.Equal(t, "job-0.image.failed", deliveries[0].WebhookID)
	assert.Equal(t, "job-0.image.failed", deliveries[0].RequestHeaders[webhook.HeaderID])
	assert.Equal(t, http.StatusNoContent, deliveries[0].Status)
	assert.Contains(t, string(deliveries[0].Body), "an older job's failure")

	// Another account sees none of it
	bob := s.signIn(t, "bob")
	s.userGet(t, bob, "/api/images", &images)
	assert.Empty(t, images)
	s.userGet(t, bob, "/api/deliveries", &deliveries)
	assert.Empty(t, deliveries)
}

// Variants are served from provino's store, and the manifests aren't
func TestMediaServesVariantsOnly(t *testing.T) {
	s := startTestServer(t)
	dir := filepath.Join(s.app.variantsDir, "alice", "0b", "8c")
	err := os.MkdirAll(dir, 0o750)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(dir, "x_600x400_3f9a0c1e.webp"), []byte("webp"), 0o600)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(dir, "x_manifest.json"), []byte("{}"), 0o600)
	require.NoError(t, err)

	res, _ := s.call(t, s.Client(), request{method: http.MethodGet, path: "/media/alice/0b/8c/x_600x400_3f9a0c1e.webp"})
	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Contains(t, res.Header.Get("Cache-Control"), "immutable")

	for _, path := range []string{"/media/alice/0b/8c/x_manifest.json", "/media/alice/0b/8c/", "/media/../app/example.db"} {
		res, _ = s.call(t, s.Client(), request{method: http.MethodGet, path: path})
		assert.NotEqual(t, http.StatusOK, res.StatusCode, path)
	}
}
