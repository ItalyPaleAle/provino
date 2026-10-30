// Package signing authenticates the requests this service sends: webhook deliveries and calls to the authorizer
//
// Every request carries the Standard Webhooks webhook-id and webhook-timestamp headers, and then, depending on the configuration, an HMAC or Ed25519 signature over its body, an OAuth2 bearer token, or nothing else
package signing

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/italypaleale/go-kit/oauth2/clientassertion"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/webhook"
)

// Message is what an outbound request authenticates
type Message struct {
	// Stable ID of the message, which stays the same across delivery attempts so the receiver can tell a retry from a new message
	ID string
	// When this attempt was made
	Timestamp time.Time
	// Exact bytes of the request body
	Body []byte
}

// Authenticator adds credentials to an outbound request
type Authenticator interface {
	// Apply sets the headers that authenticate a request carrying msg
	Apply(ctx context.Context, req *http.Request, msg Message) error
}

// New creates the authenticator one endpoint's configuration describes
// keys holds the Ed25519 signing keys and may be nil when none are configured
// timeout bounds the token requests of an OAuth2 authenticator, which use an HTTP client of their own so a slow identity provider can't consume the whole budget of the request they authenticate
func New(cfg config.ConfigOutboundAuth, keys *KeySet, timeout time.Duration) (Authenticator, error) {
	switch cfg.Type {
	case config.OutboundAuthHMAC:
		secrets := make([][]byte, 0, len(cfg.HMAC.Secrets))
		for i, secret := range cfg.HMAC.Secrets {
			key, err := webhook.DecodeSecret(secret)
			if err != nil {
				return nil, fmt.Errorf("HMAC secret %d is invalid: %w", i, err)
			}
			secrets = append(secrets, key)
		}
		return &hmacAuthenticator{secrets: secrets}, nil

	case config.OutboundAuthEd25519:
		if keys == nil {
			return nil, errors.New("no Ed25519 signing keys are configured")
		}
		return &ed25519Authenticator{keys: keys}, nil

	case config.OutboundAuthOAuth2:
		source, err := newTokenSource(cfg.OAuth2, timeout)
		if err != nil {
			return nil, err
		}
		return &oauth2Authenticator{source: source}, nil

	case config.OutboundAuthNone:
		return noneAuthenticator{}, nil

	default:
		return nil, fmt.Errorf("unsupported authentication type '%s'", cfg.Type)
	}
}

// Set the Standard Webhooks headers every request carries, whatever authenticates it
func setMessageHeaders(req *http.Request, msg Message) {
	req.Header.Set(webhook.HeaderID, msg.ID)
	req.Header.Set(webhook.HeaderTimestamp, strconv.FormatInt(msg.Timestamp.Unix(), 10))
}

// hmacAuthenticator signs with every configured secret, so a receiver rotating its secret keeps verifying throughout the rotation
type hmacAuthenticator struct {
	secrets [][]byte
}

func (a *hmacAuthenticator) Apply(_ context.Context, req *http.Request, msg Message) error {
	signatures := make([]string, len(a.secrets))
	for i, secret := range a.secrets {
		signatures[i] = webhook.SignHMAC(secret, msg.ID, msg.Timestamp, msg.Body)
	}

	setMessageHeaders(req, msg)
	req.Header.Set(webhook.HeaderSignature, strings.Join(signatures, " "))
	return nil
}

// ed25519Authenticator signs with the active key of the key set
type ed25519Authenticator struct {
	keys *KeySet
}

func (a *ed25519Authenticator) Apply(_ context.Context, req *http.Request, msg Message) error {
	setMessageHeaders(req, msg)
	req.Header.Set(webhook.HeaderSignature, webhook.SignEd25519(a.keys.active, msg.ID, msg.Timestamp, msg.Body))
	return nil
}

// oauth2Authenticator sends a client-credentials token, cached until it nears its expiry
type oauth2Authenticator struct {
	source oauth2.TokenSource
}

func (a *oauth2Authenticator) Apply(_ context.Context, req *http.Request, msg Message) error {
	// A token request that fails makes the request fail, which retries it rather than sending it unauthenticated
	token, err := a.source.Token()
	if err != nil {
		return fmt.Errorf("failed to obtain an access token: %w", err)
	}

	setMessageHeaders(req, msg)
	token.SetAuthHeader(req)
	return nil
}

// noneAuthenticator sends the message headers and nothing else
type noneAuthenticator struct{}

func (noneAuthenticator) Apply(_ context.Context, req *http.Request, msg Message) error {
	setMessageHeaders(req, msg)
	return nil
}

// Build the token source of an OAuth2 client-credentials grant
func newTokenSource(cfg config.ConfigOutboundOAuth2, timeout time.Duration) (oauth2.TokenSource, error) {
	params := url.Values{}
	for key, value := range cfg.EndpointParams {
		params.Set(key, value)
	}

	// Token requests travel over a client of their own, bound to a background context because the source outlives the request that first triggers it
	tokenCtx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: timeout})

	// A client secret is a fixed credential, so the standard grant handles it, including working out how the provider wants it sent
	if cfg.ClientSecret != "" {
		cc := &clientcredentials.Config{
			ClientID:       cfg.ClientID,
			ClientSecret:   cfg.ClientSecret,
			TokenURL:       cfg.TokenURL,
			Scopes:         cfg.Scopes,
			EndpointParams: params,
		}
		return cc.TokenSource(tokenCtx), nil
	}

	// A client assertion is obtained afresh for every token request, because the federated credential behind it is itself short-lived
	provider, err := clientassertion.NewProvider(cfg.ClientAssertion, cfg.ClientAssertionAudience)
	if err != nil {
		return nil, fmt.Errorf("failed to set up the client assertion: %w", err)
	}

	return oauth2.ReuseTokenSource(nil, &assertionTokenSource{
		ctx:      tokenCtx,
		cfg:      cfg,
		params:   params,
		provider: provider,
	}), nil
}

// assertionTokenSource requests a token authenticating with a client assertion, per RFC 7523 section 2.2
type assertionTokenSource struct {
	ctx      context.Context //nolint:containedctx // The oauth2.TokenSource interface takes no context, so the one carrying the token client is kept here
	cfg      config.ConfigOutboundOAuth2
	params   url.Values
	provider clientassertion.ProviderFn
}

func (s *assertionTokenSource) Token() (*oauth2.Token, error) {
	assertion, err := s.provider(s.ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to obtain the client assertion: %w", err)
	}

	// The assertion replaces the client secret, so it travels in the form body next to the client ID
	params := url.Values{}
	maps.Copy(params, s.params)
	params.Set("client_assertion_type", clientassertion.AssertionType)
	params.Set("client_assertion", assertion)

	cc := &clientcredentials.Config{
		ClientID:       s.cfg.ClientID,
		TokenURL:       s.cfg.TokenURL,
		Scopes:         s.cfg.Scopes,
		EndpointParams: params,
		AuthStyle:      oauth2.AuthStyleInParams,
	}
	token, err := cc.Token(s.ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to obtain an access token with a client assertion: %w", err)
	}
	return token, nil
}

// KeySet is the set of Ed25519 keys this service signs with and publishes
type KeySet struct {
	activeID string
	active   ed25519.PrivateKey
	public   []webhook.JWK
}

// LoadKeySet reads the configured Ed25519 keys, returning nil when there are none
func LoadKeySet(cfg config.ConfigSigningEd25519) (*KeySet, error) {
	if len(cfg.Keys) == 0 {
		return nil, nil //nolint:nilnil // A nil key set means none is configured, which is a valid deployment
	}

	ks := &KeySet{
		public: make([]webhook.JWK, 0, len(cfg.Keys)),
	}
	ids := make(map[string]int, len(cfg.Keys))
	for i, value := range cfg.Keys {
		private, err := webhook.ParseEd25519PrivateKey(value)
		if err != nil {
			return nil, fmt.Errorf("signing key %d is invalid: %w", i, err)
		}
		public, _ := private.Public().(ed25519.PublicKey)

		// Receivers can look keys up by ID, so two keys published with the same one would be ambiguous, and so would the same key listed twice
		id := keyID(value, public)
		other, found := ids[id]
		if found {
			return nil, fmt.Errorf("signing keys %d and %d have the same ID '%s'", other, i, id)
		}
		ids[id] = i

		// The first key signs, and every key is published, so receivers can trust a new key before it moves to the top and an old one until it's removed
		if i == 0 {
			ks.activeID = id
			ks.active = private
		}
		ks.public = append(ks.public, webhook.NewEd25519JWK(id, public))
	}

	return ks, nil
}

// Return the ID a key is published with: the `kid` of a JWK that has one, or else one derived from the public key
// The derived ID is stable for as long as the key is configured, whatever form it's written in, so receivers that cache the key set by ID keep finding it
func keyID(value string, public ed25519.PublicKey) string {
	// The value already parsed as a key, so a JSON object here is a valid JWK
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "{") {
		var jwk struct {
			Kid string `json:"kid"`
		}
		err := json.Unmarshal([]byte(value), &jwk)
		if err == nil && jwk.Kid != "" {
			return jwk.Kid
		}
	}

	sum := sha256.Sum256(public)
	return base64.RawURLEncoding.EncodeToString(sum[:7])
}

// ActiveKeyID returns the ID of the key requests are signed with
func (k *KeySet) ActiveKeyID() string {
	return k.activeID
}

// JWKS returns the published key set, encoded as JSON
func (k *KeySet) JWKS() ([]byte, error) {
	data, err := json.Marshal(webhook.JWKS{Keys: k.public})
	if err != nil {
		return nil, fmt.Errorf("failed to encode the JWKS: %w", err)
	}
	return data, nil
}
