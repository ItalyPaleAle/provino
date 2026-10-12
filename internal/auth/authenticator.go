package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/italypaleale/provino/internal/config"
)

// ErrUnauthorized is what every authentication failure reduces to
// The reason is wrapped for the logs, but it never reaches the caller: a client that can tell an expired token from a wrong audience learns something about the issuer it should not
var ErrUnauthorized = errors.New("the request is not authorized")

// Authenticator establishes who is calling
type Authenticator interface {
	// Authenticate returns the caller of a request, or an error wrapping ErrUnauthorized
	Authenticate(ctx context.Context, r *http.Request) (Principal, error)
}

// Set is the authenticator built from the configuration: JWTs and API keys side by side, or nothing at all when authentication is disabled
type Set struct {
	jwt     *Verifier
	apiKeys *APIKeys
	none    *Principal
}

// NewSet builds the authenticators the configuration enables
// The JWT verifier fetches the issuer's key set before this returns, so an unreachable issuer fails startup rather than the first request
func NewSet(ctx context.Context, cfg config.ConfigAuth, publicURL string) (*Set, error) {
	if cfg.None.Enabled {
		return &Set{none: &Principal{Subject: cfg.None.Subject, Scopes: cfg.None.Scopes, Method: "none"}}, nil
	}

	s := &Set{}
	if len(cfg.APIKeys) > 0 {
		s.apiKeys = NewAPIKeys(cfg.APIKeys)
	}
	if cfg.JWT.Issuer != "" {
		audience := cfg.JWT.Audience
		if audience == "" {
			audience = publicURL
		}

		var err error
		s.jwt, err = NewVerifier(ctx, &http.Client{Timeout: 15 * time.Second}, VerifierOptions{
			Issuer:       cfg.JWT.Issuer,
			Audience:     audience,
			JWKSURL:      cfg.JWT.JWKSURL,
			SubjectClaim: cfg.JWT.SubjectClaim,
			ScopeClaim:   cfg.JWT.ScopeClaim,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize JWT verification: %w", err)
		}
	}

	return s, nil
}

// Close stops the background refreshes of the JWT key set
func (s *Set) Close(ctx context.Context) error {
	if s.jwt == nil {
		return nil
	}
	return s.jwt.Close(ctx)
}

// Authenticate returns the caller of a request
// An API key is recognized by its prefix, so a request carrying one is never handed to the JWT verifier and vice versa
func (s *Set) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	if s.none != nil {
		return *s.none, nil
	}

	token, ok := bearerToken(r)
	if !ok {
		return Principal{}, fmt.Errorf("%w: the request has no bearer token", ErrUnauthorized)
	}

	if strings.HasPrefix(token, config.APIKeyPrefix) {
		if s.apiKeys == nil {
			return Principal{}, fmt.Errorf("%w: API keys are not enabled", ErrUnauthorized)
		}
		return s.apiKeys.Verify(token)
	}

	if s.jwt == nil {
		return Principal{}, fmt.Errorf("%w: JWTs are not enabled", ErrUnauthorized)
	}
	return s.jwt.Verify(ctx, token, time.Now())
}

// Middleware rejects every request whose caller can't be authenticated, or that doesn't hold scope
//
// The reason a request was refused is logged and never returned
func Middleware(a Authenticator, scope string, unauthorized func(w http.ResponseWriter, r *http.Request), forbidden func(w http.ResponseWriter, r *http.Request)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, err := a.Authenticate(r.Context(), r)
			if err != nil {
				slog.InfoContext(r.Context(), "Rejected a request that could not be authenticated", slog.Any("error", err))
				unauthorized(w, r)
				return
			}

			// A caller that authenticated but lacks the scope is told so, since it already proved who it is
			if !principal.HasScope(scope) {
				slog.InfoContext(r.Context(), "Rejected a request without the required scope",
					slog.String("subject", principal.Subject),
					slog.String("scope", scope),
				)
				forbidden(w, r)
				return
			}

			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

// Read the bearer token out of the Authorization header
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}

	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}

	token = strings.TrimSpace(token)

	return token, token != ""
}
