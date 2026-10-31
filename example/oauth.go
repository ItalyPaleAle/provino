package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jwk"
	"github.com/lestrrat-go/jwx/v4/jwt"
)

const (
	// Setting the signing key is stored under, as a JWK
	signingKeySetting = "signing-key"

	// Client the web UI signs users in as: a public client, without a secret
	webClientID = "example-web"
	// Client provino gets its tokens as, with any secret
	provinoClientID = "provino"

	// Scope provino requires for browser uploads, which every user token carries
	ingestScope = "ingest"

	// How long the tokens and the authorization codes are valid
	// The client-credentials token is short-lived, so a test session also sees provino fetch a new one
	userTokenLifetime    = time.Hour
	serviceTokenLifetime = 5 * time.Minute
	codeLifetime         = time.Minute
)

// account is a user who can sign in, without a password
type account struct {
	ID          string
	Name        string
	Description string
}

// The accounts that can sign in, whose IDs are the subjects of their tokens
// What each one may upload is decided by the authorizer, in provino.go
var accounts = []account{
	{ID: "alice", Name: "Alice", Description: "Can upload every format provino accepts, up to 20 MB"},
	{ID: "bob", Name: "Bob", Description: "Can upload JPEG and PNG images only, up to 2 MB"},
}

// Find an account by its ID
func findAccount(id string) (account, bool) {
	i := slices.IndexFunc(accounts, func(a account) bool { return a.ID == id })
	if i < 0 {
		return account{}, false
	}
	return accounts[i], true
}

// oauthServer is just enough of an OpenID Connect provider for provino and the web UI
//
// Browsers sign in with the authorization code grant, and get a JWT whose audience is provino, which provino verifies with the key set this server publishes
// provino gets a token with the client credentials grant, and presents it to the authorizer and the webhook endpoint
type oauthServer struct {
	issuer     string
	provinoURL string
	key        jwk.Key
	publicKeys jwk.Set
	page       *template.Template

	lock  sync.Mutex
	codes map[string]authCode
}

// authCode is an authorization code waiting to be exchanged for a token
type authCode struct {
	AccountID   string
	RedirectURI string
	Expires     time.Time
}

// Create the provider, with the signing key stored in the database, which is created the first time
func newOAuthServer(ctx context.Context, database *db, issuer string, provinoURL string) (*oauthServer, error) {
	key, err := loadSigningKey(ctx, database)
	if err != nil {
		return nil, err
	}

	public, err := jwk.PublicKeyOf(key)
	if err != nil {
		return nil, fmt.Errorf("failed to derive the public key: %w", err)
	}

	set := jwk.NewSet()
	err = set.AddKey(public)
	if err != nil {
		return nil, fmt.Errorf("failed to build the key set: %w", err)
	}

	page, err := template.ParseFS(templatesFS, "templates/signin.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse the sign-in page: %w", err)
	}

	return &oauthServer{
		issuer:     issuer,
		provinoURL: provinoURL,
		key:        key,
		publicKeys: set,
		page:       page,
		codes:      make(map[string]authCode),
	}, nil
}

// Load the Ed25519 key tokens are signed with, or create it
// The key is kept across restarts, so the tokens already issued, and the key set provino fetched, stay valid
func loadSigningKey(ctx context.Context, database *db) (jwk.Key, error) {
	stored, found, err := database.setting(ctx, signingKeySetting)
	if err != nil {
		return nil, err
	}

	if found {
		key, err := jwk.ParseKey([]byte(stored))
		if err != nil {
			return nil, fmt.Errorf("failed to parse the stored signing key: %w", err)
		}
		return key, nil
	}

	// Create a new key, named after its thumbprint, and declare its algorithm, so the key set says how tokens are signed
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate the signing key: %w", err)
	}
	key, err := jwk.Import[jwk.Key](private)
	if err != nil {
		return nil, fmt.Errorf("failed to import the signing key: %w", err)
	}
	err = jwk.AssignKeyID(key)
	if err != nil {
		return nil, fmt.Errorf("failed to name the signing key: %w", err)
	}
	err = key.Set(jwk.AlgorithmKey, jwa.EdDSA())
	if err != nil {
		return nil, fmt.Errorf("failed to set the signing key's algorithm: %w", err)
	}

	data, err := json.Marshal(key)
	if err != nil {
		return nil, fmt.Errorf("failed to encode the signing key: %w", err)
	}

	err = database.setSetting(ctx, signingKeySetting, string(data))
	if err != nil {
		return nil, err
	}

	return key, nil
}

// Publish the discovery document, which is how provino finds the key set
func (s *oauthServer) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.issuer,
		"authorization_endpoint":                s.issuer + "/oauth/authorize",
		"token_endpoint":                        s.issuer + "/oauth/token",
		"jwks_uri":                              s.issuer + "/.well-known/jwks.json",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "client_credentials"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic", "client_secret_post"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"EdDSA"},
		"scopes_supported":                      []string{ingestScope},
	})
}

// Publish the public key tokens are verified with
func (s *oauthServer) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.publicKeys)
}

// authorizeParams are the parameters of an authorization request, which the sign-in page carries through to its form
type authorizeParams struct {
	ClientID    string
	RedirectURI string
	State       string
}

// Read and check the parameters of an authorization request, from the query or from the sign-in form
// The web UI is the only client that signs users in, so the redirect has to be its callback
func (s *oauthServer) authorizeParams(r *http.Request) (authorizeParams, error) {
	p := authorizeParams{
		ClientID:    r.Form.Get("client_id"),
		RedirectURI: r.Form.Get("redirect_uri"),
		State:       r.Form.Get("state"),
	}
	switch {
	case r.Form.Get("response_type") != "code":
		return p, errors.New("response_type must be 'code'")
	case p.ClientID != webClientID:
		return p, fmt.Errorf("client_id must be '%s'", webClientID)
	case p.RedirectURI != s.issuer+"/callback":
		return p, fmt.Errorf("redirect_uri must be '%s/callback'", s.issuer)
	}
	return p, nil
}

// Show the sign-in page, which lists the accounts
func (s *oauthServer) handleAuthorizePage(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	params, err := s.authorizeParams(r)
	if err != nil {
		http.Error(w, "Invalid authorization request: "+err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = s.page.Execute(w, map[string]any{
		"Params":   params,
		"Accounts": accounts,
	})
}

// Sign in as the account picked on the sign-in page, and send the browser back to the web UI with an authorization code
func (s *oauthServer) handleAuthorizeSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	_ = r.ParseForm()
	params, err := s.authorizeParams(r)
	if err != nil {
		http.Error(w, "Invalid authorization request: "+err.Error(), http.StatusBadRequest)
		return
	}
	acct, ok := findAccount(r.PostForm.Get("account"))
	if !ok {
		http.Error(w, "Unknown account", http.StatusBadRequest)
		return
	}

	// The code is single-use and short-lived, and is exchanged for a token only with the redirect URI it was sent to
	code := randomToken()
	s.lock.Lock()
	now := time.Now()
	for c, pending := range s.codes {
		if now.After(pending.Expires) {
			delete(s.codes, c)
		}
	}
	s.codes[code] = authCode{
		AccountID:   acct.ID,
		RedirectURI: params.RedirectURI,
		Expires:     now.Add(codeLifetime),
	}
	s.lock.Unlock()

	query := url.Values{"code": {code}}
	if params.State != "" {
		query.Set("state", params.State)
	}

	http.Redirect(w, r, params.RedirectURI+"?"+query.Encode(), http.StatusSeeOther)
}

// Issue a token, for an authorization code from the web UI or for provino's client credentials
func (s *oauthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	err := r.ParseForm()
	if err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request", "the body isn't a valid form")
		return
	}

	// A confidential client may authenticate with HTTP Basic or in the form, which is what golang.org/x/oauth2 tries in turn
	clientID, clientSecret, basic := r.BasicAuth()
	if !basic {
		clientID = r.PostForm.Get("client_id")
		clientSecret = r.PostForm.Get("client_secret")
	}

	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r, clientID)
	case "client_credentials":
		s.clientCredentials(w, r, clientID, clientSecret)
	default:
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "the grant type must be 'authorization_code' or 'client_credentials'")
	}
}

// Exchange an authorization code for the token of the account that signed in
func (s *oauthServer) exchangeCode(w http.ResponseWriter, r *http.Request, clientID string) {
	if clientID != webClientID {
		tokenError(w, http.StatusUnauthorized, "invalid_client", "only the web UI signs users in")
		return
	}

	// The code is removed as it's read, so it can be exchanged once only
	code := r.PostForm.Get("code")
	s.lock.Lock()
	pending, found := s.codes[code]
	delete(s.codes, code)
	s.lock.Unlock()
	if !found || time.Now().After(pending.Expires) || pending.RedirectURI != r.PostForm.Get("redirect_uri") {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "the code is unknown, expired, already used, or was issued for another redirect URI")
		return
	}

	acct, _ := findAccount(pending.AccountID)
	token, err := s.issueUserToken(acct)
	if err != nil {
		tokenError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeToken(w, token, userTokenLifetime, ingestScope)
}

// Issue provino a token for calling the app, whatever secret it presents
func (s *oauthServer) clientCredentials(w http.ResponseWriter, r *http.Request, clientID string, clientSecret string) {
	if clientID != provinoClientID || clientSecret == "" {
		tokenError(w, http.StatusUnauthorized, "invalid_client", fmt.Sprintf("only the client '%s' can use client credentials, with any non-empty secret", provinoClientID))
		return
	}

	scope := r.PostForm.Get("scope")
	token, err := s.sign(jwt.NewBuilder().
		Subject(provinoClientID).
		Audience([]string{s.issuer}).
		Claim("azp", provinoClientID).
		Claim("client_id", provinoClientID).
		Claim("scope", scope), serviceTokenLifetime)
	if err != nil {
		tokenError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	writeToken(w, token, serviceTokenLifetime, scope)
}

// Issue the token a signed-in user presents to provino, and to the app's own API
func (s *oauthServer) issueUserToken(acct account) (string, error) {
	return s.sign(jwt.NewBuilder().
		Subject(acct.ID).
		Audience([]string{s.provinoURL, s.issuer}).
		Claim("azp", webClientID).
		Claim("scope", ingestScope).
		Claim("name", acct.Name), userTokenLifetime)
}

// Sign a token built by the caller, adding the claims every token has
func (s *oauthServer) sign(builder *jwt.Builder, lifetime time.Duration) (string, error) {
	now := time.Now()
	token, err := builder.
		Issuer(s.issuer).
		IssuedAt(now).
		Expiration(now.Add(lifetime)).
		JwtID(randomToken()).
		Build()
	if err != nil {
		return "", fmt.Errorf("failed to build the token: %w", err)
	}

	// Signing with the JWK puts its key ID in the header, which is how a verifier picks the key out of the set
	signed, err := jwt.Sign(token, jwt.WithKey(jwa.EdDSA(), s.key))
	if err != nil {
		return "", fmt.Errorf("failed to sign the token: %w", err)
	}
	return string(signed), nil
}

// Verify a token this server issued for an audience and a client, returning its subject
func (s *oauthServer) verify(raw string, audience string, clientID string) (string, error) {
	token, err := jwt.ParseString(raw,
		jwt.WithKeySet(s.publicKeys),
		jwt.WithIssuer(s.issuer),
		jwt.WithAudience(audience),
		jwt.WithValidate(true),
	)
	if err != nil {
		return "", fmt.Errorf("the token did not verify: %w", err)
	}

	// Checking the client matters, since a user's token has this app in its audience too
	azp, err := jwt.Get[string](token, "azp")
	if err != nil || azp != clientID {
		return "", fmt.Errorf("the token wasn't issued to '%s'", clientID)
	}
	subject, _ := token.Subject()
	return subject, nil
}

// Read the bearer token of a request
func bearerToken(r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token, ok && token != ""
}

// Answer a token request with a token
func writeToken(w http.ResponseWriter, token string, lifetime time.Duration, scope string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int(lifetime.Seconds()),
		"scope":        scope,
	})
}

// Answer a token request with an OAuth2 error
func tokenError(w http.ResponseWriter, status int, code string, description string) {
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": description,
	})
}

// Return a random, URL-safe string to use as a code or a token ID
func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
