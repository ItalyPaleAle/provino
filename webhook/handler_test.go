package webhook

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const authorizeBody = `{"version":"provino/v1","subject":"user_1","context":{"site":"a"}}`

type authorizeFunc = func(r *http.Request, ar *AuthorizeRequest) (*AuthorizeResponse, bool, error)

func newRequest(t *testing.T, body string) *http.Request {
	t.Helper()

	return httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
}

// Build a request signed with the reference vector's secret, timestamped now
func signedRequest(t *testing.T, body string) *http.Request {
	t.Helper()

	key, err := DecodeSecret(vectorSecret)
	require.NoError(t, err)
	now := time.Now()
	req := newRequest(t, body)
	req.Header = signedHeader("msg_1", now, SignHMAC(key, "msg_1", now, []byte(body)))
	return req
}

func serveAuthorize(r *http.Request, v *Verifier, fn authorizeFunc) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	HandleAuthorize(v, fn).ServeHTTP(rec, r)
	return rec
}

func serveEvent(r *http.Request, v *Verifier, fn func(r *http.Request, e *Event) error) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	HandleEvent(v, fn).ServeHTTP(rec, r)
	return rec
}

func TestHandleAuthorize(t *testing.T) {
	t.Run("answers 200 with the decision", func(t *testing.T) {
		var received *AuthorizeRequest
		rec := serveAuthorize(newRequest(t, authorizeBody), nil, func(_ *http.Request, ar *AuthorizeRequest) (*AuthorizeResponse, bool, error) {
			received = ar
			return &AuthorizeResponse{Namespace: "a", Notify: &Notify{Endpoint: "app"}}, true, nil
		})
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		assert.JSONEq(t, `{"namespace":"a","notify":{"endpoint":"app"}}`, rec.Body.String())

		require.NotNil(t, received)
		assert.Equal(t, "user_1", received.Subject)
		assert.JSONEq(t, `{"site":"a"}`, string(received.Context))
	})

	t.Run("answers 403 when the callback refuses", func(t *testing.T) {
		rec := serveAuthorize(newRequest(t, authorizeBody), nil, func(*http.Request, *AuthorizeRequest) (*AuthorizeResponse, bool, error) {
			return nil, false, nil
		})
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("answers 500 when the callback fails, without sending the error", func(t *testing.T) {
		rec := serveAuthorize(newRequest(t, authorizeBody), nil, func(*http.Request, *AuthorizeRequest) (*AuthorizeResponse, bool, error) {
			return &AuthorizeResponse{Namespace: "a"}, true, errors.New("database password is hunter2")
		})
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "hunter2")
	})

	t.Run("answers 500 when the callback allows without a response", func(t *testing.T) {
		rec := serveAuthorize(newRequest(t, authorizeBody), nil, func(*http.Request, *AuthorizeRequest) (*AuthorizeResponse, bool, error) {
			return nil, true, nil
		})
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	t.Run("refuses a request it can't decode, without calling the callback", func(t *testing.T) {
		mustNotBeCalled := func(*http.Request, *AuthorizeRequest) (*AuthorizeResponse, bool, error) {
			t.Fatal("the callback must not be called")
			return nil, false, nil
		}

		tests := map[string]struct {
			body   string
			status int
		}{
			"invalid JSON":            {body: `{"version":`, status: http.StatusBadRequest},
			"another version":         {body: `{"version":"provino/v2","subject":"user_1"}`, status: http.StatusBadRequest},
			"no version":              {body: `{"subject":"user_1"}`, status: http.StatusBadRequest},
			"a body larger than 1 MB": {body: `{"version":"provino/v1","subject":"` + strings.Repeat("a", maxBodyBytes) + `"}`, status: http.StatusRequestEntityTooLarge},
		}
		for name, tc := range tests {
			t.Run(name, func(t *testing.T) {
				rec := serveAuthorize(newRequest(t, tc.body), nil, mustNotBeCalled)
				assert.Equal(t, tc.status, rec.Code)
			})
		}
	})

	t.Run("with a verifier", func(t *testing.T) {
		v, err := NewVerifier(t.Context(), WithHMACSecrets(vectorSecret))
		require.NoError(t, err)

		t.Run("answers a signed request", func(t *testing.T) {
			rec := serveAuthorize(signedRequest(t, authorizeBody), v, func(_ *http.Request, ar *AuthorizeRequest) (*AuthorizeResponse, bool, error) {
				return &AuthorizeResponse{Namespace: "a"}, true, nil
			})
			assert.Equal(t, http.StatusOK, rec.Code)
		})

		t.Run("answers 401 without calling the callback when the signature doesn't match", func(t *testing.T) {
			mustNotBeCalled := func(*http.Request, *AuthorizeRequest) (*AuthorizeResponse, bool, error) {
				t.Fatal("the callback must not be called")
				return nil, false, nil
			}

			// The signature covers the exact bytes, so even a change in whitespace breaks it
			tampered := signedRequest(t, authorizeBody)
			tampered.Body = newRequest(t, authorizeBody+" ").Body
			rec := serveAuthorize(tampered, v, mustNotBeCalled)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)

			rec = serveAuthorize(newRequest(t, authorizeBody), v, mustNotBeCalled)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	})
}

func TestHandleEvent(t *testing.T) {
	const body = `{"type":"image.failed","timestamp":"2026-10-11T10:11:14Z","data":{"namespace":"a","imageId":"0b8cb0f7-2a1a-4f0f-9a9e-2b3a3f2f5f2a","error":"image_not_found"}}`

	t.Run("acknowledges with 204", func(t *testing.T) {
		var received *Event
		rec := serveEvent(newRequest(t, body), nil, func(_ *http.Request, e *Event) error {
			received = e
			return nil
		})
		require.Equal(t, http.StatusNoContent, rec.Code)
		assert.Empty(t, rec.Body.String())

		require.NotNil(t, received)
		assert.Equal(t, EventImageFailed, received.Type)
		assert.True(t, received.Timestamp.Equal(time.Date(2026, 10, 11, 10, 11, 14, 0, time.UTC)))
		assert.Equal(t, "image_not_found", received.Data.Error)
	})

	t.Run("answers 500 when the callback fails, so provino retries", func(t *testing.T) {
		rec := serveEvent(newRequest(t, body), nil, func(*http.Request, *Event) error {
			return errors.New("database is down")
		})
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "database")
	})

	t.Run("refuses invalid JSON, without calling the callback", func(t *testing.T) {
		rec := serveEvent(newRequest(t, `not json`), nil, func(*http.Request, *Event) error {
			t.Fatal("the callback must not be called")
			return nil
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("with a verifier", func(t *testing.T) {
		v, err := NewVerifier(t.Context(), WithHMACSecrets(vectorSecret))
		require.NoError(t, err)

		// The callback gets the request, whose webhook-id identifies the delivery
		var deliveryID string
		rec := serveEvent(signedRequest(t, body), v, func(r *http.Request, _ *Event) error {
			deliveryID = r.Header.Get(HeaderID)
			return nil
		})
		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, "msg_1", deliveryID)

		rec = serveEvent(newRequest(t, body), v, func(*http.Request, *Event) error {
			t.Fatal("the callback must not be called")
			return nil
		})
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}
