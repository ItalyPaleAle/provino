// Package notify delivers the webhook events a job sends when it finishes
//
// Endpoints are named in the configuration and requests pick one by name, so a URL never comes from a caller and there is nothing a request can point this service at
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/internal/signing"
	"github.com/italypaleale/provino/webhook"
)

// How much of a response is read, which bounds what a misbehaving receiver can make this service allocate while still keeping an error message readable in the logs
const maxResponseRead = 8 << 10 // 8KB

// MaxContextSize is the largest context a notification may carry
const MaxContextSize = 4 << 10 // 4KB

var (
	// ErrUnknownEndpoint is returned for a notification that names an endpoint that isn't configured
	ErrUnknownEndpoint = errors.New("the webhook endpoint is not configured")
	// ErrNoDefaultEndpoint is returned for a notification that names no endpoint when there is no default one
	ErrNoDefaultEndpoint = errors.New("no webhook endpoint is named and there is no default one")
	// ErrPermanent marks a delivery that can never succeed, so retrying it is pointless
	ErrPermanent = errors.New("the delivery cannot succeed")
)

// Notifier holds the configured webhook endpoints
type Notifier struct {
	endpoints       map[string]*endpoint
	defaultEndpoint string
	now             func() time.Time
}

type endpoint struct {
	url    string
	auth   signing.Authenticator
	client *http.Client
}

// New creates the endpoints the configuration describes
func New(cfg config.ConfigNotify, keys *signing.KeySet) (*Notifier, error) {
	n := &Notifier{
		endpoints:       make(map[string]*endpoint, len(cfg.Endpoints)),
		defaultEndpoint: cfg.DefaultEndpoint,
		now:             time.Now,
	}

	for name, ep := range cfg.Endpoints {
		auth, err := signing.New(ep.Auth, keys, ep.Timeout)
		if err != nil {
			return nil, fmt.Errorf("webhook endpoint '%s': %w", name, err)
		}

		n.endpoints[name] = &endpoint{
			url:  ep.URL,
			auth: auth,
			client: &http.Client{
				Timeout: ep.Timeout,
				// A redirect is answered as the response it is, so a receiver can't send a delivery anywhere the configuration didn't name
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
		}
	}

	return n, nil
}

// Endpoints returns the names of the configured endpoints, sorted
func (n *Notifier) Endpoints() []string {
	names := make([]string, 0, len(n.endpoints))
	for name := range n.endpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Resolve returns the endpoint a notification is delivered to: the one it names, or the default one when it names none
func (n *Notifier) Resolve(name string) (string, error) {
	if name == "" {
		if n.defaultEndpoint == "" {
			return "", ErrNoDefaultEndpoint
		}
		return n.defaultEndpoint, nil
	}

	_, found := n.endpoints[name]
	if !found {
		return "", fmt.Errorf("%w: '%s'", ErrUnknownEndpoint, name)
	}
	return name, nil
}

// ValidateContext checks the context a notification carries: absent, or a JSON object of at most MaxContextSize bytes
func ValidateContext(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if len(trimmed) > MaxContextSize {
		return fmt.Errorf("the context is larger than %d bytes", MaxContextSize)
	}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return errors.New("the context must be a JSON object")
	}
	return nil
}

// Deliver posts an event to an endpoint
//
// deliveryID is the Standard Webhooks message ID, which stays the same across attempts so the receiver can tell a retry from a new event
// Only a 2xx response acknowledges the delivery; anything else is an error, so the job step retries it
func (n *Notifier) Deliver(ctx context.Context, endpointName string, deliveryID string, event webhook.Event) error {
	ep, found := n.endpoints[endpointName]
	if !found {
		return fmt.Errorf("%w: %w: '%s'", ErrPermanent, ErrUnknownEndpoint, endpointName)
	}

	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("%w: failed to encode the event: %w", ErrPermanent, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: failed to create the request: %w", ErrPermanent, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "provino-webhook/1")

	// The timestamp is the attempt's, not the event's, because a receiver refuses one too far from its clock and a retry can come long after the event
	err = ep.auth.Apply(ctx, req, signing.Message{ID: deliveryID, Timestamp: n.now(), Body: body})
	if err != nil {
		return fmt.Errorf("failed to authenticate the delivery: %w", err)
	}

	res, err := ep.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to deliver the event: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck

	if res.StatusCode >= http.StatusOK && res.StatusCode < http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxResponseRead))
		return nil
	}

	detail, _ := io.ReadAll(io.LimitReader(res.Body, maxResponseRead))
	return fmt.Errorf("the endpoint answered %s: %s", res.Status, strings.TrimSpace(string(detail)))
}
