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
	"sync/atomic"
	"time"

	"github.com/jwx-go/jwkfetch/v4"
	"github.com/lestrrat-go/httprc/v3"
	"github.com/lestrrat-go/httprc/v3/errsink"
	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jwk"
)

// DefaultTolerance is how far a request's timestamp may be from the receiver's clock before the request is refused as a replay
const DefaultTolerance = 5 * time.Minute

// Bounds on fetching a JWKS, so a misbehaving server can't make a receiver allocate or wait without limit
const (
	jwksFetchTimeout = 10 * time.Second
	jwksMaxBodySize  = 64 << 10 // 64KB
	// A signature that matches no known key triggers a refresh, but no more often than this, so forged requests can't turn the receiver into a load generator against provino
	// It's also the shortest interval the background refresh honors when the server asks for a shorter one
	jwksMinRefreshInterval = time.Minute
	// Keys are refreshed in the background at least this often even when every signature matches, so a revoked key stops being accepted
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
	jwksURL     string
	jwksClient  *http.Client
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
// The key set is fetched by NewVerifier and refreshed in the background, at least every hour, until Close is called
// A signature that matches no known key fetches it again early, which is what picks up a rotated key, and a refresh that fails keeps the keys fetched last
// If client is nil, http.DefaultClient is used; either way redirects can't downgrade to plain HTTP, and a client without a timeout gets one
func WithJWKS(url string, client *http.Client) VerifierOption {
	return func(v *Verifier) error {
		if url == "" {
			return errors.New("the JWKS URL is empty")
		}
		if client == nil {
			client = http.DefaultClient
		}
		v.jwksURL = url
		v.jwksClient = jwkfetch.WrapHTTPClientDefaults(client)
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
// ctx bounds that first fetch only, and Close stops the background refreshes that follow it
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

	if len(v.hmacKeys) == 0 && len(v.ed25519Keys) == 0 && v.jwksURL == "" {
		return nil, errors.New("the verifier needs at least one HMAC secret, Ed25519 public key, or JWKS")
	}

	if v.jwksURL != "" {
		var err error
		v.jwks, err = newJWKSSource(ctx, v.jwksURL, v.jwksClient, v.now())
		if err != nil {
			return nil, err
		}
	}

	return v, nil
}

// Close stops the background refreshes of the JWKS, and does nothing for a verifier without one
func (v *Verifier) Close(ctx context.Context) error {
	if v.jwks == nil {
		return nil
	}
	return v.jwks.close(ctx)
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

	// The cached key set is the last one fetched successfully, so a background refresh that failed still leaves the keys that were known
	if v.matches(content, signatures, v.jwks.keys(ctx)) {
		return nil
	}

	// A miss may be the first request signed with a rotated key, so it fetches the set again, as long as no other miss did within the minimum interval
	refreshed, err := v.jwks.refreshOnMiss(ctx, now)
	if err != nil {
		return errors.Join(ErrInvalidSignature, err)
	}
	if refreshed && v.matches(content, signatures, v.jwks.keys(ctx)) {
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

// jwksSource keeps the Ed25519 keys of a JWKS, which a jwkfetch cache refreshes in the background
// The cache serves the last set it fetched successfully, so neither a failed refresh nor a lock held during a fetch stands between a request and the keys already known
type jwksSource struct {
	url   string
	cache *jwkfetch.Cache
	// When the last refresh triggered by a miss was attempted, as Unix nanoseconds, whether it succeeded or not
	lastRefresh atomic.Int64
}

// Fetch the key set and keep it refreshed until close is called
func newJWKSSource(ctx context.Context, url string, client *http.Client, now time.Time) (*jwksSource, error) {
	// Give up on the first fetch after its timeout, and stop waiting as soon as a fetch reports why it failed
	readyCtx, cancelReady := context.WithTimeout(ctx, jwksFetchTimeout)
	defer cancelReady()
	fetchErrCh := make(chan error, 1)
	fetchErrSink := errsink.NewFunc(func(_ context.Context, fetchErr error) {
		// Only the first error matters: it's the one that explains why the set never became ready
		select {
		case fetchErrCh <- fetchErr:
			cancelReady()
		default:
		}
	})

	// The refreshes outlive the context of the first fetch, so they stop on close rather than when the caller's context ends
	cache, err := jwkfetch.NewCache(
		context.WithoutCancel(ctx),
		httprc.NewClient(
			httprc.WithHTTPClient(client),
			httprc.WithWhitelist(httprc.NewMapWhitelist().Add(url)),
			httprc.WithErrorSink(fetchErrSink),
		),
		jwkfetch.WithHTTPClient(client),
		jwkfetch.WithMaxBodySize(jwksMaxBodySize),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create the JWKS cache: %w", err)
	}

	// Block until the first set is ready, so a receiver that can't reach provino fails at startup
	err = cache.Register(readyCtx, url,
		jwkfetch.WithWaitReady(true),
		jwkfetch.WithMinInterval(jwksMinRefreshInterval),
		jwkfetch.WithMaxInterval(jwksMaxAge),
	)
	if err != nil {
		// Report what the fetch failed on rather than only that the set never became ready
		select {
		case fetchErr := <-fetchErrCh:
			err = fetchErr
		default:
		}

		// The caller's context may be what ended the wait, and shutting the cache down still needs a live deadline
		shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer shutdownCancel()
		shutdownErr := cache.Shutdown(shutdownCtx)
		return nil, errors.Join(fmt.Errorf("failed to fetch the JWKS: %w", err), shutdownErr)
	}

	s := &jwksSource{url: url, cache: cache}

	// The first fetch counts as an attempt, so a miss right after startup doesn't fetch the set again
	s.lastRefresh.Store(now.UnixNano())

	return s, nil
}

// Return the Ed25519 keys of the last set fetched successfully
func (s *jwksSource) keys(ctx context.Context) []ed25519.PublicKey {
	set, err := s.cache.Lookup(ctx, s.url)
	if err != nil {
		return nil
	}
	return ed25519KeysOf(set)
}

// Fetch the set again after a signature matched no known key, reporting whether a fetch was made
// Attempts rather than successes are spaced out, so neither forged signatures nor an unreachable server can make every request fetch the set
func (s *jwksSource) refreshOnMiss(ctx context.Context, now time.Time) (bool, error) {
	last := s.lastRefresh.Load()
	if now.Sub(time.Unix(0, last)) < jwksMinRefreshInterval {
		return false, nil
	}

	// When several misses arrive together, one of them fetches and the others don't wait for it
	if !s.lastRefresh.CompareAndSwap(last, now.UnixNano()) {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(ctx, jwksFetchTimeout)
	defer cancel()
	_, err := s.cache.Refresh(ctx, s.url)
	if err != nil {
		return true, fmt.Errorf("failed to refresh the JWKS: %w", err)
	}
	return true, nil
}

// Stop the background refreshes
func (s *jwksSource) close(ctx context.Context) error {
	err := s.cache.Shutdown(ctx)
	if err != nil {
		return fmt.Errorf("failed to stop the JWKS cache: %w", err)
	}
	return nil
}

// Extract the Ed25519 public keys of a set, ignoring keys of any other type
func ed25519KeysOf(set jwk.Set) []ed25519.PublicKey {
	keys := make([]ed25519.PublicKey, 0, set.Len())
	for i := range set.Len() {
		key, ok := set.Key(i)
		if !ok || key.KeyType() != jwa.OKP() {
			continue
		}
		okp, ok := key.(jwk.OKPPublicKey)
		if !ok {
			continue
		}
		curve, ok := okp.Crv()
		if !ok || curve != jwa.Ed25519() {
			continue
		}
		raw, err := jwk.Export[ed25519.PublicKey](key)
		if err != nil {
			continue
		}
		keys = append(keys, raw)
	}
	return keys
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
