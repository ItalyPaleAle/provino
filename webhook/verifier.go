package webhook

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultTolerance is how far a request's timestamp may be from the receiver's clock before the request is refused as a replay
const DefaultTolerance = 5 * time.Minute

// Bounds on fetching a JWKS, so a misbehaving server can't make a receiver allocate or wait without limit
const (
	jwksFetchTimeout = 10 * time.Second
	// A signature that matches no known key triggers a refresh, but no more often than this, so forged requests can't turn the receiver into a load generator against provino
	jwksMinRefreshInterval = time.Minute
	// Keys are refreshed at least this often even when every signature matches, so a revoked key stops being accepted
	jwksMaxAge = time.Hour
)

var (
	// ErrMissingHeaders is returned when a request lacks one of the Standard Webhooks headers
	ErrMissingHeaders = errors.New("the request is missing the webhook-id, webhook-timestamp, or webhook-signature header")
	// ErrTimestampOutOfRange is returned when a request's timestamp is further from the receiver's clock than the tolerance
	ErrTimestampOutOfRange = errors.New("the request's timestamp is outside the allowed tolerance")
	// ErrInvalidSignature is returned when no signature on the request matches a known key
	ErrInvalidSignature = errors.New("the request's signature does not match")
)

// Verifier checks the signatures on requests provino sends: webhook events, and calls to the authorizer
type Verifier struct {
	hmacKeys    [][]byte
	ed25519Keys []ed25519.PublicKey
	jwks        *jwksSource
	tolerance   time.Duration
	now         func() time.Time
}

// VerifierOption configures a Verifier
type VerifierOption func(*Verifier) error

// WithHMACSecrets accepts `v1` signatures made with any of the secrets, in the Standard Webhooks `whsec_` form
// Listing more than one is how a receiver rotates a secret without refusing deliveries signed with the old one
func WithHMACSecrets(secrets ...string) VerifierOption {
	return func(v *Verifier) error {
		for i, secret := range secrets {
			key, err := DecodeSecret(secret)
			if err != nil {
				return fmt.Errorf("HMAC secret %d: %w", i, err)
			}
			v.hmacKeys = append(v.hmacKeys, key)
		}
		return nil
	}
}

// WithEd25519PublicKeys accepts `v1a` signatures made with any of the keys
func WithEd25519PublicKeys(keys ...ed25519.PublicKey) VerifierOption {
	return func(v *Verifier) error {
		for i, key := range keys {
			if len(key) != ed25519.PublicKeySize {
				return fmt.Errorf("Ed25519 public key %d is not %d bytes long", i, ed25519.PublicKeySize)
			}
			v.ed25519Keys = append(v.ed25519Keys, key)
		}
		return nil
	}
}

// WithJWKS accepts `v1a` signatures made with any Ed25519 key in the JWKS provino publishes at `/.well-known/provino-keys.json`
// The key set is fetched by NewVerifier, refreshed every hour, and fetched again early when a signature matches no known key, which is what picks up a rotated key
// If client is nil, http.DefaultClient is used
func WithJWKS(url string, client *http.Client) VerifierOption {
	return func(v *Verifier) error {
		if client == nil {
			client = http.DefaultClient
		}
		v.jwks = &jwksSource{url: url, client: client}
		return nil
	}
}

// WithTolerance changes how far a request's timestamp may be from the receiver's clock, which defaults to DefaultTolerance
func WithTolerance(tolerance time.Duration) VerifierOption {
	return func(v *Verifier) error {
		if tolerance <= 0 {
			return errors.New("the tolerance must be positive")
		}
		v.tolerance = tolerance
		return nil
	}
}

// NewVerifier creates a Verifier
// When it's configured with WithJWKS, the key set is fetched before this returns, so a receiver that can't reach provino fails at startup rather than on its first request
func NewVerifier(ctx context.Context, opts ...VerifierOption) (*Verifier, error) {
	v := &Verifier{
		tolerance: DefaultTolerance,
		now:       time.Now,
	}
	for _, opt := range opts {
		err := opt(v)
		if err != nil {
			return nil, err
		}
	}

	if len(v.hmacKeys) == 0 && len(v.ed25519Keys) == 0 && v.jwks == nil {
		return nil, errors.New("the verifier needs at least one HMAC secret, Ed25519 public key, or JWKS")
	}

	if v.jwks != nil {
		_, err := v.jwks.refresh(ctx, v.now())
		if err != nil {
			return nil, err
		}
	}

	return v, nil
}

// Verify checks the Standard Webhooks headers of a request against its body
// The body must be the exact bytes that were received, before any decoding
func (v *Verifier) Verify(ctx context.Context, header http.Header, body []byte) error {
	id := header.Get(HeaderID)
	timestampHeader := header.Get(HeaderTimestamp)
	signatureHeader := header.Get(HeaderSignature)
	if id == "" || timestampHeader == "" || signatureHeader == "" {
		return ErrMissingHeaders
	}

	// The timestamp is checked before any key is tried, which is what keeps a captured request from being replayed later
	seconds, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: it is not a Unix timestamp", ErrTimestampOutOfRange)
	}
	timestamp := time.Unix(seconds, 0)
	now := v.now()
	if timestamp.Before(now.Add(-v.tolerance)) || timestamp.After(now.Add(v.tolerance)) {
		return ErrTimestampOutOfRange
	}

	content := signedContent(id, timestamp, body)
	signatures := strings.Fields(signatureHeader)
	if v.matches(content, signatures, v.ed25519Keys) {
		return nil
	}
	if v.jwks == nil {
		return ErrInvalidSignature
	}

	// The cached key set is tried first, and a miss refreshes it, because the miss may be the first request signed with a rotated key
	keys := v.jwks.cached(now)
	if v.matches(content, signatures, keys) {
		return nil
	}
	keys, err = v.jwks.refresh(ctx, now)
	if err != nil {
		return errors.Join(ErrInvalidSignature, err)
	}
	if v.matches(content, signatures, keys) {
		return nil
	}

	return ErrInvalidSignature
}

// VerifyRequest reads up to maxBytes of a request's body and verifies it, returning the body so the caller can decode it
func (v *Verifier) VerifyRequest(r *http.Request, maxBytes int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read the request body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("the request body is larger than %d bytes", maxBytes)
	}

	err = v.Verify(r.Context(), r.Header, body)
	if err != nil {
		return nil, err
	}

	return body, nil
}

// Report whether any signature in the header matches one of the keys
func (v *Verifier) matches(content []byte, signatures []string, ed25519Keys []ed25519.PublicKey) bool {
	for _, entry := range signatures {
		version, encoded, ok := strings.Cut(entry, ",")
		if !ok {
			continue
		}
		signature, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}

		switch version {
		case signatureVersionHMAC:
			for _, key := range v.hmacKeys {
				mac := hmac.New(sha256.New, key)
				mac.Write(content)
				if hmac.Equal(signature, mac.Sum(nil)) {
					return true
				}
			}
		case signatureVersionEd25519:
			for _, key := range ed25519Keys {
				if ed25519.Verify(key, content, signature) {
					return true
				}
			}
		}
	}

	return false
}

// jwksSource keeps the Ed25519 keys of a JWKS, refreshing them when they're stale or when a signature matches none of them
type jwksSource struct {
	url    string
	client *http.Client

	mu        sync.Mutex
	keys      []ed25519.PublicKey
	fetchedAt time.Time
}

// Return the cached keys, or nil when they're older than the maximum age, which makes the caller refresh them
func (s *jwksSource) cached(now time.Time) []ed25519.PublicKey {
	s.mu.Lock()
	defer s.mu.Unlock()

	if now.Sub(s.fetchedAt) > jwksMaxAge {
		return nil
	}
	return s.keys
}

// Fetch the key set again, unless it was fetched less than the minimum interval ago
func (s *jwksSource) refresh(ctx context.Context, now time.Time) ([]ed25519.PublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.fetchedAt.IsZero() && now.Sub(s.fetchedAt) < jwksMinRefreshInterval {
		return s.keys, nil
	}

	keys, err := FetchJWKS(ctx, s.client, s.url)
	if err != nil {
		return s.keys, err
	}
	s.keys = keys
	s.fetchedAt = now

	return keys, nil
}

// JWK is one key in a JWKS, as provino publishes its Ed25519 signing keys
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid,omitempty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
}

// JWKS is a JSON Web Key Set
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// NewEd25519JWK describes an Ed25519 public key as a JWK
func NewEd25519JWK(kid string, key ed25519.PublicKey) JWK {
	return JWK{
		Kty: "OKP",
		Crv: "Ed25519",
		X:   base64.RawURLEncoding.EncodeToString(key),
		Kid: kid,
		Use: "sig",
		Alg: "EdDSA",
	}
}

// Ed25519Keys returns the Ed25519 public keys in the set, ignoring keys of any other type
func (s JWKS) Ed25519Keys() ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(s.Keys))
	for i, jwk := range s.Keys {
		if jwk.Kty != "OKP" || jwk.Crv != "Ed25519" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("key %d is not a valid Ed25519 public key", i)
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys, nil
}

// FetchJWKS downloads a JWKS and returns the Ed25519 keys in it
func FetchJWKS(ctx context.Context, client *http.Client, url string) ([]ed25519.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, jwksFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create the JWKS request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch the JWKS: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching the JWKS returned %s", res.Status)
	}

	var set JWKS
	err = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&set)
	if err != nil {
		return nil, fmt.Errorf("failed to decode the JWKS: %w", err)
	}

	keys, err := set.Ed25519Keys()
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, errors.New("the JWKS holds no Ed25519 keys")
	}

	return keys, nil
}
