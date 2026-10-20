//go:build unit

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCORSAllowsConfiguredOrigins(t *testing.T) {
	handler := middlewareCORS([]string{"https://app.example", "https://allowed.example"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))

	request := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/public/ingest", nil)
	request.Header.Set("Origin", "https://allowed.example")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusNoContent, recorder.Code)
	assert.Equal(t, "https://allowed.example", recorder.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, recorder.Header().Get("Access-Control-Allow-Headers"), "Authorization")
	// The preflight's answer is cached, so the browser doesn't ask again before every request
	assert.Equal(t, "7200", recorder.Header().Get("Access-Control-Max-Age"))

	// The cache lifetime only means something on a preflight
	request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/public/ingest", nil)
	request.Header.Set("Origin", "https://allowed.example")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusAccepted, recorder.Code)
	assert.Empty(t, recorder.Header().Get("Access-Control-Max-Age"))
}

func TestCORSRefusesOtherOrigins(t *testing.T) {
	handler := middlewareCORS([]string{"https://app.example", "https://allowed.example"})(http.NotFoundHandler())
	request := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/public/ingest", nil)
	request.Header.Set("Origin", "https://other.example")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusForbidden, recorder.Code)
	assert.Empty(t, recorder.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, recorder.Header().Get("Access-Control-Max-Age"))
}
