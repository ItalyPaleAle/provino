package authorizer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/variant"
	"github.com/italypaleale/provino/webhook"
)

const testSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw" //nolint:gosec // A test fixture, not a credential

var testImages = config.ConfigImages{MaxVariants: 48, MaxResolution: 8192}

// Start a fake authorizer that verifies the request the way an application would, then answers with handler
func newHTTPAuthorizer(t *testing.T, handler func(req webhook.AuthorizeRequest, w http.ResponseWriter)) Authorizer {
	t.Helper()

	verifier, err := webhook.NewVerifier(t.Context(), webhook.WithHMACSecrets(testSecret))
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := verifier.VerifyRequest(r, 1<<20)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		assert.True(t, strings.HasPrefix(r.Header.Get(webhook.HeaderID), "authz_"))

		var req webhook.AuthorizeRequest
		err = json.Unmarshal(body, &req)
		assert.NoError(t, err)
		handler(req, w)
	}))
	t.Cleanup(server.Close)

	a, err := New(config.ConfigAuthorizer{Type: config.AuthorizerHTTP, HTTP: config.ConfigAuthorizerHTTP{
		URL:     server.URL,
		Timeout: 5 * time.Second,
		Auth:    config.ConfigOutboundAuth{Type: config.OutboundAuthHMAC, HMAC: config.ConfigOutboundHMAC{Secrets: []string{testSecret}}},
	}}, testImages, nil)
	require.NoError(t, err)
	return a
}

func answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) //nolint:errchkjson // The test values always encode
}

func TestHTTPAuthorizer(t *testing.T) {
	imageID := uuid.NewV4()

	t.Run("sends the contract and validates the answer", func(t *testing.T) {
		a := newHTTPAuthorizer(t, func(req webhook.AuthorizeRequest, w http.ResponseWriter) {
			assert.Equal(t, webhook.AuthorizeVersion, req.Version)
			assert.Equal(t, "user-1", req.Subject)
			assert.JSONEq(t, `{"site":"s1","profile":"hero"}`, string(req.Context))

			answer(w, http.StatusOK, webhook.AuthorizeResponse{
				Namespace:      "s1",
				ImageID:        imageID.String(),
				MaxBytes:       1 << 20,
				AllowedFormats: []string{"JPEG", "png", "tiff"},
				Variants:       []variant.Spec{{Format: "webp", Quality: 80, Size: variant.Resolution{Width: 500, Height: 500}}},
				Notify:         &webhook.Notify{Endpoint: "app", Context: json.RawMessage(`{"k":"v"}`)},
			})
		})

		decision, err := a.Authorize(t.Context(), Request{Subject: "user-1", Context: json.RawMessage(`{"site":"s1","profile":"hero"}`)})
		require.NoError(t, err)
		assert.Equal(t, "s1", decision.Namespace)
		assert.Equal(t, imageID, decision.ImageID)
		assert.Equal(t, int64(1<<20), decision.MaxBytes)
		// Unknown formats are dropped and aliases resolved
		assert.Equal(t, []string{"jpg", "png"}, decision.AllowedFormats)
		require.Len(t, decision.Variants, 1)
		assert.Equal(t, variant.FitContain, decision.Variants[0].Fit)
		require.NotNil(t, decision.Notify)
		assert.Equal(t, "app", decision.Notify.Endpoint)
	})

	// New images get a v4 ID, but an image that already has a v7 one keeps it
	t.Run("an answer with a v7 image ID keeps it", func(t *testing.T) {
		v7ImageID := uuid.NewV7()
		a := newHTTPAuthorizer(t, func(_ webhook.AuthorizeRequest, w http.ResponseWriter) {
			answer(w, http.StatusOK, webhook.AuthorizeResponse{Namespace: "default", ImageID: v7ImageID.String()})
		})

		decision, err := a.Authorize(t.Context(), Request{Subject: "user-1"})
		require.NoError(t, err)
		assert.Equal(t, v7ImageID, decision.ImageID)
	})

	t.Run("an answer without an image ID or formats leaves both to this service", func(t *testing.T) {
		a := newHTTPAuthorizer(t, func(_ webhook.AuthorizeRequest, w http.ResponseWriter) {
			answer(w, http.StatusOK, webhook.AuthorizeResponse{Namespace: "default"})
		})

		decision, err := a.Authorize(t.Context(), Request{Subject: "user-1"})
		require.NoError(t, err)
		assert.Equal(t, uuid.Nil(), decision.ImageID)
		assert.Equal(t, config.SourceFormats, decision.AllowedFormats)
		assert.Empty(t, decision.Variants)
	})

	t.Run("a 403 is a refusal", func(t *testing.T) {
		a := newHTTPAuthorizer(t, func(_ webhook.AuthorizeRequest, w http.ResponseWriter) {
			answer(w, http.StatusForbidden, map[string]string{"error": "no"})
		})
		_, err := a.Authorize(t.Context(), Request{Subject: "user-1"})
		require.ErrorIs(t, err, ErrRefused)
	})

	// A mistyped URL answers 404, which has to surface as a failure rather than as every caller being refused
	t.Run("a 404 is a failure that points at the URL", func(t *testing.T) {
		a := newHTTPAuthorizer(t, func(_ webhook.AuthorizeRequest, w http.ResponseWriter) {
			http.NotFound(w, nil)
		})
		_, err := a.Authorize(t.Context(), Request{Subject: "user-1"})
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrRefused)
		assert.ErrorContains(t, err, "authorizer.http.url")
	})

	failures := map[string]func(w http.ResponseWriter){
		"a server error": func(w http.ResponseWriter) { answer(w, http.StatusInternalServerError, map[string]string{}) },
		"a redirect": func(w http.ResponseWriter) {
			w.Header().Set("Location", "https://elsewhere.example")
			w.WriteHeader(http.StatusFound)
		},
		"invalid JSON":    func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK); _, _ = io.WriteString(w, "{") },
		"no namespace":    func(w http.ResponseWriter) { answer(w, http.StatusOK, webhook.AuthorizeResponse{}) },
		"a bad namespace": func(w http.ResponseWriter) { answer(w, http.StatusOK, webhook.AuthorizeResponse{Namespace: "../x"}) },
		// The DNS namespace UUID of RFC 9562, which is a v1
		"a v1 image ID": func(w http.ResponseWriter) {
			answer(w, http.StatusOK, webhook.AuthorizeResponse{Namespace: "d", ImageID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		},
		"a negative limit": func(w http.ResponseWriter) {
			answer(w, http.StatusOK, webhook.AuthorizeResponse{Namespace: "d", MaxBytes: -1})
		},
		"no usable format": func(w http.ResponseWriter) {
			answer(w, http.StatusOK, webhook.AuthorizeResponse{Namespace: "d", AllowedFormats: []string{"gif"}})
		},
		"an invalid variant": func(w http.ResponseWriter) {
			answer(w, http.StatusOK, webhook.AuthorizeResponse{Namespace: "d", Variants: []variant.Spec{{Format: "webp"}}})
		},
		"an oversized answer": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"namespace":"d","pad":"`+strings.Repeat("x", maxResponseSize)+`"}`)
		},
	}
	for name, respond := range failures {
		t.Run(name+" is a failure, not a refusal", func(t *testing.T) {
			a := newHTTPAuthorizer(t, func(_ webhook.AuthorizeRequest, w http.ResponseWriter) {
				respond(w)
			})
			_, err := a.Authorize(t.Context(), Request{Subject: "user-1"})
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrRefused)
		})
	}
}

func TestStaticAuthorizer(t *testing.T) {
	a, err := New(config.ConfigAuthorizer{Type: config.AuthorizerStatic, Static: config.ConfigAuthorizerStatic{
		Namespace:      "default",
		MaxBytes:       1 << 20,
		AllowedFormats: []string{"jpg"},
		Variants:       []variant.Spec{{Format: "jpg", Quality: 80, Size: variant.Resolution{Width: 800}}},
		Notify:         &config.ConfigNotifyTarget{Endpoint: "app", Context: map[string]any{"source": "static"}},
	}}, testImages, nil)
	require.NoError(t, err)

	decision, err := a.Authorize(t.Context(), Request{Subject: "anyone"})
	require.NoError(t, err)
	assert.Equal(t, "default", decision.Namespace)
	assert.Equal(t, uuid.Nil(), decision.ImageID)
	assert.Equal(t, []string{"jpg"}, decision.AllowedFormats)
	assert.Equal(t, variant.FormatJPEG, decision.Variants[0].Format)
	require.NotNil(t, decision.Notify)
	assert.JSONEq(t, `{"source":"static"}`, string(decision.Notify.Context))

	// A caller changing the decision it got doesn't change the next one
	decision.Variants[0].Quality = 1
	again, err := a.Authorize(t.Context(), Request{Subject: "anyone"})
	require.NoError(t, err)
	assert.Equal(t, 80, again.Variants[0].Quality)
}

func TestNewWithoutAnAuthorizer(t *testing.T) {
	a, err := New(config.ConfigAuthorizer{}, testImages, nil)
	require.NoError(t, err)
	assert.Nil(t, a)
}
