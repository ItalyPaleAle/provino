package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jwx-go/jwkfetch/v4"
	"github.com/lestrrat-go/httprc/v3"
	"github.com/lestrrat-go/httprc/v3/errsink"
	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jwk"
	"github.com/lestrrat-go/jwx/v4/jws"
	"github.com/lestrrat-go/jwx/v4/jwt"
)

// Leeway allowed on the token's expiry, which absorbs a small clock difference between the identity provider and this service
const clockSkew = 30 * time.Second

// Maximum size of the documents fetched from the issuer
const maxDocumentSize = 128 << 10 // 128KB

// Bound on the initial key-set fetch, because the cache's ready wait ends only when its context does and its refresh loop retries a failing key set forever
const keySetReadyTimeout = 30 * time.Second

// VerifierOptions configures a Verifier
type VerifierOptions struct {
	// Issuer whose tokens are accepted
	Issuer string
	// Expected audience
	Audience string
	// URL of the JWKS, which skips OpenID Connect discovery when set
	JWKSURL string
	// Claim holding the subject
	SubjectClaim string
	// Claim holding the scopes, as a space-separated string or an array
	ScopeClaim string
}

// Verifier checks JWTs against the keys the identity provider publishes
// The key set is refreshed in the background so the provider can rotate its signing keys without restarting this service
type Verifier struct {
	// issuer is the value tokens carry in `iss`, taken verbatim from the discovery document when there is one
	// Providers commonly declare an issuer with a trailing slash, which a token's claim then has to match exactly
	issuer       string
	audience     string
	jwksURI      string
	subjectClaim string
	scopeClaim   string
	cache        *jwkfetch.Cache
}

// NewVerifier locates the issuer's key set, fetches it before startup completes, and keeps it refreshed
func NewVerifier(ctx context.Context, client *http.Client, opts VerifierOptions) (*Verifier, error) {
	// Validate the trust boundary before creating a background cache
	issuer := strings.TrimSpace(opts.Issuer)
	issuerURL, err := parseIssuer(issuer)
	if err != nil {
		return nil, err
	}
	if opts.Audience == "" {
		return nil, errors.New("the audience is empty")
	}
	if client == nil {
		client = http.DefaultClient
	}

	// An operator that names the key set skips discovery, which is how providers without OpenID Connect metadata are supported
	// A discovered key set is held to the issuer's origin instead, so the metadata can't point the verifier anywhere else
	declaredIssuer := issuer
	var jwksURI string
	if opts.JWKSURL != "" {
		jwksURI = opts.JWKSURL
	} else {
		client = sameOriginHTTPClient(client, issuerURL)
		declaredIssuer, jwksURI, err = discover(ctx, client, issuer, issuerURL)
		if err != nil {
			return nil, err
		}
	}

	cache, err := startKeySetCache(ctx, client, jwksURI)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch the key set of '%s': %w", issuer, err)
	}

	subjectClaim := opts.SubjectClaim
	if subjectClaim == "" {
		subjectClaim = "sub"
	}
	scopeClaim := opts.ScopeClaim
	if scopeClaim == "" {
		scopeClaim = "scope"
	}

	return &Verifier{
		issuer:       declaredIssuer,
		audience:     opts.Audience,
		jwksURI:      jwksURI,
		subjectClaim: subjectClaim,
		scopeClaim:   scopeClaim,
		cache:        cache,
	}, nil
}

// Read the issuer's discovery document, returning the issuer it declares and the key set it names
func discover(ctx context.Context, client *http.Client, issuer string, issuerURL *url.URL) (declaredIssuer string, jwksURI string, err error) {
	base := strings.TrimRight(issuer, "/")

	var metadata struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"` //nolint:tagliatelle // The name is the one the OpenID discovery specification mandates
	}
	err = fetchJSON(ctx, client, base+"/.well-known/openid-configuration", &metadata)
	if err != nil {
		return "", "", fmt.Errorf("failed to fetch the discovery document of '%s': %w", issuer, err)
	}

	// Refuse metadata that could redirect verification away from the configured issuer
	// The comparison ignores a trailing slash so an issuer can be configured either way, but the declared form is what tokens are checked against
	if strings.TrimRight(metadata.Issuer, "/") != base {
		return "", "", fmt.Errorf("the discovery document of '%s' declares a different issuer '%s'", issuer, metadata.Issuer)
	}
	if metadata.JWKSURI == "" {
		return "", "", fmt.Errorf("the discovery document of '%s' names no key set", issuer)
	}

	jwksURL, err := parseIssuerResource(metadata.JWKSURI, issuerURL)
	if err != nil {
		return "", "", fmt.Errorf("the discovery document of '%s' names an unsafe key set: %w", issuer, err)
	}

	return metadata.Issuer, jwksURL.String(), nil
}

// Start the background cache of a key set, returning once the first fetch has succeeded
func startKeySetCache(ctx context.Context, client *http.Client, jwksURI string) (*jwkfetch.Cache, error) {
	// Give up on the initial key set once the deadline passes, and stop waiting as soon as a refresh reports why it cannot be fetched
	readyCtx, cancelReady := context.WithTimeout(ctx, keySetReadyTimeout)
	defer cancelReady()
	refreshErrCh := make(chan error, 1)
	refreshErrSink := errsink.NewFunc(func(_ context.Context, refreshErr error) {
		// Only the first error matters: it is the one that explains why startup never saw a key set
		select {
		case refreshErrCh <- refreshErr:
			cancelReady()
		default:
		}
	})

	// Start one refresh cache and allow only the exact resource validated above
	cache, err := jwkfetch.NewCache(
		ctx,
		httprc.NewClient(
			httprc.WithHTTPClient(client),
			httprc.WithWhitelist(httprc.NewMapWhitelist().Add(jwksURI)),
			httprc.WithErrorSink(refreshErrSink),
		),
		jwkfetch.WithHTTPClient(client),
		jwkfetch.WithMaxBodySize(maxDocumentSize),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create the key cache: %w", err)
	}

	// Block until the initial key set is ready so an unreachable issuer fails startup rather than the first request
	err = cache.Register(
		readyCtx,
		jwksURI,
		jwkfetch.WithWaitReady(true),
		jwkfetch.WithMinInterval(15*time.Minute),
		jwkfetch.WithMaxInterval(24*time.Hour),
	)
	if err != nil {
		// Report what the fetch failed on rather than only that the key set never became ready
		select {
		case refreshErr := <-refreshErrCh:
			err = refreshErr
		default:
		}

		// Registration can fail because the startup context was canceled, but cache shutdown still needs a live deadline to stop its workers
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		shutdownErr := cache.Shutdown(shutdownCtx) //nolint:contextcheck
		return nil, errors.Join(err, shutdownErr)
	}

	return cache, nil
}

// Close stops background key-set refreshes
func (v *Verifier) Close(ctx context.Context) error {
	err := v.cache.Shutdown(ctx)
	if err != nil {
		return fmt.Errorf("failed to shut down the key cache: %w", err)
	}
	return nil
}

// Issuer returns the identifier tokens must be issued by
func (v *Verifier) Issuer() string {
	return v.issuer
}

// Verify checks a compact JWS and returns the caller it authenticates
// The signing algorithm is checked as service policy before the library verifies the signature, because an algorithm read from an unverified header is a value the caller chose
func (v *Verifier) Verify(ctx context.Context, token string, now time.Time) (Principal, error) {
	// Inspect only protected headers so the algorithm cannot be supplied outside the signature
	message, err := jws.ParseString(token, jws.WithCompact())
	if err != nil {
		return Principal{}, fmt.Errorf("%w: the token is not a compact JWS: %w", ErrUnauthorized, err)
	}
	signatures := message.Signatures()
	if len(signatures) != 1 {
		return Principal{}, fmt.Errorf("%w: the token does not contain exactly one signature", ErrUnauthorized)
	}
	protected := signatures[0].ProtectedHeaders()
	if protected == nil {
		return Principal{}, fmt.Errorf("%w: the token has no protected header", ErrUnauthorized)
	}

	// Which algorithm a token uses is up to the issuer and its key set
	// Only accept asymettric algorithms (and reject "none")
	algorithm, ok := protected.Algorithm()
	if !ok || algorithm.IsSymmetric() || algorithm == jwa.NoSignature() {
		return Principal{}, fmt.Errorf("%w: the token is not signed with an asymmetric algorithm", ErrUnauthorized)
	}

	// Read the current cached set so key rotation takes effect without rebuilding the verifier
	set, err := v.cache.Lookup(ctx, v.jwksURI)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: failed to read the issuer's key set: %w", ErrUnauthorized, err)
	}

	// Only public keys take part, so a symmetric key published in the set can never verify anything, whatever a token's header claims
	// The algorithm is inferred from a key that doesn't declare one, which is what lets the key set decide the algorithms rather than this service
	_, err = jwt.ParseString(
		token,
		jwt.WithContext(ctx),
		jwt.WithKeySet(asymmetricKeys(set), jws.WithInferAlgorithmFromKey(true)),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithRequiredClaim(jwt.ExpirationKey),
		jwt.WithAcceptableSkew(clockSkew),
		jwt.WithClock(jwt.ClockFunc(func() time.Time { return now })),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: the token did not verify: %w", ErrUnauthorized, err)
	}

	// Decode the original payload only after the library has established trust in those exact bytes
	var claims map[string]any
	err = json.Unmarshal(message.Payload(), &claims)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: the verified claims are malformed: %w", ErrUnauthorized, err)
	}
	subject, _ := claims[v.subjectClaim].(string)
	if strings.TrimSpace(subject) == "" {
		return Principal{}, fmt.Errorf("%w: the token names no subject in claim '%s'", ErrUnauthorized, v.subjectClaim)
	}

	return Principal{
		Subject: subject,
		Scopes:  scopesFromClaim(claims[v.scopeClaim]),
		Method:  "jwt",
	}, nil
}

// Keep the keys of a set that can verify an asymmetric signature
func asymmetricKeys(set jwk.Set) jwk.Set {
	out := jwk.NewSet()
	for i := range set.Len() {
		key, ok := set.Key(i)
		if !ok {
			continue
		}
		switch key.KeyType() {
		case jwa.RSA(), jwa.EC(), jwa.OKP():
			_ = out.AddKey(key)
		}
	}
	return out
}

// Read scopes from a claim that is either a space-separated string, which is what OAuth2 servers use, or an array, which is what some providers use instead
func scopesFromClaim(value any) []string {
	switch v := value.(type) {
	case string:
		return strings.Fields(v)
	case []any:
		scopes := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if ok && s != "" {
				scopes = append(scopes, s)
			}
		}
		return scopes
	default:
		return nil
	}
}

func fetchJSON(ctx context.Context, client *http.Client, url string, dest any) error {
	// Bind discovery to startup cancellation so a stalled issuer cannot block shutdown
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create the request: %w", err)
	}

	// Accept only successful, bounded JSON responses from the configured issuer
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to perform the request: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("the request returned %s", res.Status)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxDocumentSize))
	if err != nil {
		return fmt.Errorf("failed to read the response: %w", err)
	}

	err = json.Unmarshal(body, dest)
	if err != nil {
		return fmt.Errorf("failed to parse the response: %w", err)
	}

	return nil
}

// Parse the configured issuer, whose origin is the only one discovery is allowed to contact
// The issuer may have a path, as it does for providers that host several tenants or realms on one origin
func parseIssuer(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("the issuer is empty")
	}

	issuer, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the issuer is not a valid URL: %w", err)
	}
	if !issuer.IsAbs() || (issuer.Scheme != "https" && issuer.Scheme != "http") || issuer.Hostname() == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return nil, errors.New("the issuer must be an absolute HTTP URL without a query or fragment")
	}

	return issuer, nil
}

// Parse a discovered resource without allowing metadata to expand the configured network boundary
func parseIssuerResource(raw string, issuer *url.URL) (*url.URL, error) {
	resource, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("the URL is invalid: %w", err)
	}
	if !resource.IsAbs() || resource.Hostname() == "" || resource.User != nil || resource.Fragment != "" {
		return nil, errors.New("the URL must be an absolute HTTP URL without credentials or a fragment")
	}
	if !sameOrigin(resource, issuer) {
		return nil, errors.New("the URL must use the issuer's origin")
	}

	return resource, nil
}

// Clone the caller's client so redirects cannot escape the configured issuer while preserving its transport and timeout
func sameOriginHTTPClient(client *http.Client, issuer *url.URL) *http.Client {
	restricted := *client
	originalCheckRedirect := client.CheckRedirect
	restricted.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(req.URL, issuer) {
			return errors.New("redirects may not leave the issuer's origin")
		}
		if originalCheckRedirect != nil {
			return originalCheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &restricted
}

func sameOrigin(left *url.URL, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectivePort(left) == effectivePort(right)
}

func effectivePort(value *url.URL) string {
	port := value.Port()
	if port != "" {
		return port
	}
	if strings.EqualFold(value.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(value.Scheme, "http") {
		return "80"
	}
	return ""
}
