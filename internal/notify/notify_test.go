package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/webhook"
)

const testSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw" //nolint:gosec // A test fixture, not a credential

func newNotifier(t *testing.T, endpoints map[string]string, defaultEndpoint string) *Notifier {
	t.Helper()

	cfg := config.ConfigNotify{DefaultEndpoint: defaultEndpoint, Endpoints: map[string]config.ConfigNotifyEndpoint{}}
	for name, url := range endpoints {
		cfg.Endpoints[name] = config.ConfigNotifyEndpoint{
			URL:     url,
			Timeout: 5 * time.Second,
			Auth:    config.ConfigOutboundAuth{Type: config.OutboundAuthHMAC, HMAC: config.ConfigOutboundHMAC{Secrets: []string{testSecret}}},
		}
	}

	n, err := New(cfg, nil)
	require.NoError(t, err)
	return n
}

func TestDeliverSendsASignedEvent(t *testing.T) {
	verifier, err := webhook.NewVerifier(t.Context(), webhook.WithHMACSecrets(testSecret))
	require.NoError(t, err)

	received := make(chan webhook.Event, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "job-1.image.optimized", r.Header.Get(webhook.HeaderID))

		body, err := verifier.VerifyRequest(r, 1<<20)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var event webhook.Event
		err = json.Unmarshal(body, &event)
		assert.NoError(t, err)
		received <- event
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)

	n := newNotifier(t, map[string]string{"app": receiver.URL}, "app")
	event := webhook.Event{
		Type:      webhook.EventImageOptimized,
		Timestamp: time.Now().UTC().Truncate(time.Second),
		Data:      webhook.EventData{Namespace: "default", ImageID: "img", JobID: "job-1", Context: json.RawMessage(`{"site":"x"}`)},
	}
	err = n.Deliver(t.Context(), "app", "job-1.image.optimized", event)
	require.NoError(t, err)

	got := <-received
	assert.Equal(t, event.Type, got.Type)
	assert.Equal(t, "job-1", got.Data.JobID)
	assert.JSONEq(t, `{"site":"x"}`, string(got.Data.Context))
}

func TestDeliverFailsOnAnythingButSuccess(t *testing.T) {
	t.Run("an error status", func(t *testing.T) {
		receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "try later", http.StatusServiceUnavailable)
		}))
		t.Cleanup(receiver.Close)

		n := newNotifier(t, map[string]string{"app": receiver.URL}, "")
		err := n.Deliver(t.Context(), "app", "id", webhook.Event{})
		require.ErrorContains(t, err, "503")
		require.ErrorContains(t, err, "try later")
		require.NotErrorIs(t, err, ErrPermanent)
	})

	// A redirect is not followed, so a receiver can't move a delivery to a URL the configuration doesn't name
	t.Run("a redirect", func(t *testing.T) {
		var followed atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			followed.Store(true)
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(target.Close)
		receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(receiver.Close)

		n := newNotifier(t, map[string]string{"app": receiver.URL}, "")
		err := n.Deliver(t.Context(), "app", "id", webhook.Event{})
		require.ErrorContains(t, err, "307")
		assert.False(t, followed.Load())
	})

	t.Run("an unknown endpoint is permanent", func(t *testing.T) {
		n := newNotifier(t, map[string]string{}, "")
		err := n.Deliver(t.Context(), "missing", "id", webhook.Event{})
		require.ErrorIs(t, err, ErrPermanent)
	})

	// Only a bounded part of an error response is read, whatever the receiver sends
	t.Run("a huge error body", func(t *testing.T) {
		receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, strings.Repeat("x", 1<<20))
		}))
		t.Cleanup(receiver.Close)

		n := newNotifier(t, map[string]string{"app": receiver.URL}, "")
		err := n.Deliver(t.Context(), "app", "id", webhook.Event{})
		require.Error(t, err)
		assert.Less(t, len(err.Error()), maxResponseRead+200)
	})
}

func TestResolve(t *testing.T) {
	n := newNotifier(t, map[string]string{"app": "https://a.example.com", "other": "https://b.example.com"}, "app")

	name, err := n.Resolve("")
	require.NoError(t, err)
	assert.Equal(t, "app", name)

	name, err = n.Resolve("other")
	require.NoError(t, err)
	assert.Equal(t, "other", name)

	_, err = n.Resolve("missing")
	require.ErrorIs(t, err, ErrUnknownEndpoint)

	noDefault := newNotifier(t, map[string]string{"app": "https://a.example.com", "other": "https://b.example.com"}, "")
	_, err = noDefault.Resolve("")
	require.ErrorIs(t, err, ErrNoDefaultEndpoint)

	assert.Equal(t, []string{"app", "other"}, n.Endpoints())
}

func TestValidateContext(t *testing.T) {
	for _, valid := range []string{"", "null", `{}`, `{"site":"x","n":[1,2]}`} {
		err := ValidateContext(json.RawMessage(valid))
		require.NoError(t, err, valid)
	}

	for _, invalid := range []string{`[]`, `"text"`, `42`, `{"unterminated":`, `{"big":"` + strings.Repeat("x", MaxContextSize) + `"}`} {
		err := ValidateContext(json.RawMessage(invalid))
		require.Error(t, err, invalid)
	}
}
