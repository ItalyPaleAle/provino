// Package authorizer decides where a browser upload goes and what it may produce, before a byte of the upload is stored
//
// Asking at the moment of the upload, rather than reading a decision out of the token, is what keeps it current: a revoked permission or a changed variant policy takes effect on the next upload
package authorizer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/italypaleale/provino/imagekeys"
	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/internal/signing"
	"github.com/italypaleale/provino/internal/sniff"
	"github.com/italypaleale/provino/variant"
	"github.com/italypaleale/provino/webhook"
)

// Maximum size of the authorizer's answer, which bounds what a misbehaving authorizer can make this service allocate
const maxResponseSize = 256 << 10 // 256KB

// ErrRefused reports that the authorizer refused the upload rather than failing to answer
// It is separate from a failure because a refusal is a decision about the caller, while a failure is this service's problem
var ErrRefused = errors.New("the authorizer refused the upload")

// Request describes an upload the authorizer is asked about
type Request struct {
	// Subject of the caller
	Subject string
	// Opaque context the caller sent in the upload's metadata
	Context json.RawMessage
}

// Decision is the authorizer's answer about an upload it allows, validated
type Decision struct {
	// Namespace the image is stored in
	Namespace string
	// ID of the image, or the zero UUID when this service mints one
	ImageID uuid.UUID
	// Maximum size of the upload, or 0 when only this service's own limit applies
	MaxBytes int64
	// Source formats the upload may be in, never empty
	AllowedFormats []string
	// Variants to generate, in canonical form
	Variants []variant.Spec
	// Where the job reports its outcome, or nil
	Notify *webhook.Notify
}

// Authorizer decides what an upload may do
type Authorizer interface {
	// Authorize returns the decision about an upload, ErrRefused when the upload isn't allowed, or another error when no decision could be made
	Authorize(ctx context.Context, req Request) (Decision, error)
}

// New creates the authorizer the configuration describes, or returns nil when browser uploads are disabled
func New(cfg config.ConfigAuthorizer, images config.ConfigImages, keys *signing.KeySet) (Authorizer, error) {
	switch cfg.Type {
	case "":
		return nil, nil //nolint:nilnil // A nil authorizer means browser uploads are disabled, which is a valid deployment

	case config.AuthorizerHTTP:
		auth, err := signing.New(cfg.HTTP.Auth, keys, cfg.HTTP.Timeout)
		if err != nil {
			return nil, fmt.Errorf("failed to set up the authorizer's authentication: %w", err)
		}
		return &httpAuthorizer{
			url:    cfg.HTTP.URL,
			auth:   auth,
			images: images,
			client: &http.Client{
				Timeout: cfg.HTTP.Timeout,
				// A redirect is answered as the response it is, which is a failure, so the authorizer can't send this service anywhere the configuration didn't name
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
		}, nil

	case config.AuthorizerStatic:
		static := cfg.Static
		response := webhook.AuthorizeResponse{
			Namespace:      static.Namespace,
			MaxBytes:       static.MaxBytes,
			AllowedFormats: static.AllowedFormats,
			Variants:       static.Variants,
		}
		if static.Notify != nil {
			notifyContext, err := json.Marshal(static.Notify.Context)
			if err != nil {
				return nil, fmt.Errorf("failed to encode the static authorizer's notification context: %w", err)
			}
			if static.Notify.Context == nil {
				notifyContext = nil
			}
			response.Notify = &webhook.Notify{
				Endpoint: static.Notify.Endpoint,
				Context:  notifyContext,
			}
		}

		decision, err := validate(response, images)
		if err != nil {
			return nil, fmt.Errorf("the static authorizer's decision is invalid: %w", err)
		}
		return staticAuthorizer{decision: decision}, nil

	default:
		return nil, fmt.Errorf("unsupported authorizer type '%s'", cfg.Type)
	}
}

// staticAuthorizer applies the same decision to every upload
type staticAuthorizer struct {
	decision Decision
}

func (a staticAuthorizer) Authorize(context.Context, Request) (Decision, error) {
	// The slices are copied so a caller can't change the decision for the uploads that follow
	d := a.decision
	d.AllowedFormats = slices.Clone(d.AllowedFormats)
	d.Variants = slices.Clone(d.Variants)
	return d, nil
}

// httpAuthorizer asks an endpoint the operator runs
type httpAuthorizer struct {
	url    string
	auth   signing.Authenticator
	client *http.Client
	images config.ConfigImages
}

func (a *httpAuthorizer) Authorize(ctx context.Context, req Request) (Decision, error) {
	body, err := json.Marshal(webhook.AuthorizeRequest{
		Version: webhook.AuthorizeVersion,
		Subject: req.Subject,
		Context: req.Context,
	})
	if err != nil {
		return Decision{}, fmt.Errorf("failed to encode the authorization request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(body))
	if err != nil {
		return Decision{}, fmt.Errorf("failed to create the authorization request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "provino-authorizer/1")

	// Each request is a new message, so it gets an ID of its own that a receiver can log or deduplicate on
	err = a.auth.Apply(ctx, httpReq, signing.Message{ID: "authz_" + uuid.NewV4().String(), Timestamp: time.Now(), Body: body})
	if err != nil {
		return Decision{}, fmt.Errorf("failed to authenticate the authorization request: %w", err)
	}

	res, err := a.client.Do(httpReq)
	if err != nil {
		return Decision{}, fmt.Errorf("failed to reach the authorizer: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck

	// A refusal is the authorizer's answer about this caller, so it's reported as one rather than as a failure to reach it
	// Only a 403 refuses: a 404 is what a mistyped URL gets, and reading it as a refusal would turn a misconfiguration into every caller being refused with nothing in the logs
	if res.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxResponseSize))
		return Decision{}, ErrRefused
	}
	if res.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		hint := ""
		if res.StatusCode == http.StatusNotFound {
			hint = " (check that 'authorizer.http.url' names the authorizer's path)"
		}
		return Decision{}, fmt.Errorf("the authorizer answered %s%s: %s", res.Status, hint, strings.TrimSpace(string(detail)))
	}

	// Reading one byte past the limit is what tells an answer that's exactly at the limit apart from one that's over it
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseSize+1))
	if err != nil {
		return Decision{}, fmt.Errorf("failed to read the authorizer's answer: %w", err)
	}
	if len(data) > maxResponseSize {
		return Decision{}, fmt.Errorf("the authorizer's answer is larger than %d bytes", maxResponseSize)
	}

	var response webhook.AuthorizeResponse
	err = json.Unmarshal(data, &response)
	if err != nil {
		return Decision{}, fmt.Errorf("failed to decode the authorizer's answer: %w", err)
	}

	decision, err := validate(response, a.images)
	if err != nil {
		return Decision{}, fmt.Errorf("the authorizer's answer is invalid: %w", err)
	}
	return decision, nil
}

// Check an answer describes an upload this service can carry out, and put it in canonical form
func validate(response webhook.AuthorizeResponse, images config.ConfigImages) (Decision, error) {
	err := imagekeys.ValidateNamespace(response.Namespace)
	if err != nil {
		return Decision{}, fmt.Errorf("invalid namespace: %w", err)
	}

	decision := Decision{Namespace: response.Namespace, Notify: response.Notify}

	// A new image always gets a v4 ID, which is what keeps the storage folders balanced
	if response.ImageID != "" {
		decision.ImageID, err = imagekeys.ValidateID(response.ImageID)
		if err != nil {
			return Decision{}, fmt.Errorf("invalid image ID: %w", err)
		}
	}

	if response.MaxBytes < 0 {
		return Decision{}, errors.New("maxBytes must not be negative")
	}
	decision.MaxBytes = response.MaxBytes

	// Formats this service doesn't know are dropped rather than refused, so an authorizer can name a format a newer version supports, but a list with nothing usable left allows nothing
	if len(response.AllowedFormats) == 0 {
		decision.AllowedFormats = slices.Clone(config.SourceFormats)
	} else {
		for _, format := range response.AllowedFormats {
			normalized, ok := sniff.NormalizeFormat(format)
			if ok && !slices.Contains(decision.AllowedFormats, normalized) {
				decision.AllowedFormats = append(decision.AllowedFormats, normalized)
			}
		}
		if len(decision.AllowedFormats) == 0 {
			return Decision{}, fmt.Errorf("allowedFormats names none of the supported formats (%s)", strings.Join(config.SourceFormats, ", "))
		}
	}

	decision.Variants, err = variant.NormalizeSet(response.Variants, images.MaxResolution, images.MaxVariants)
	if err != nil {
		return Decision{}, fmt.Errorf("invalid variants: %w", err)
	}

	return decision, nil
}
