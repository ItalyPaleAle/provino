package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/italypaleale/provino/variant"
	"github.com/italypaleale/provino/webhook"
)

// Name of the webhook endpoint in example/provino.yaml, which the authorizer asks provino to report to
const notifyEndpoint = "example"

// Largest request provino sends the app that's recorded, far more than an authorization request or an event takes
const maxCapturedBody = 1 << 20

// profile is a set of variants the web UI can upload with
type profile struct {
	Label       string
	Description string
	Variants    []variant.Spec
}

// The profiles, in the order the web UI lists them
var profileOrder = []string{"photo", "avatar"}

// The variants each profile generates, which are hardcoded here, so a browser can only pick between them
var profiles = map[string]profile{
	"photo": {
		Label:       "Photo",
		Description: "WebP at 480 and 1200 pixels, and AVIF at 1200 pixels, keeping the shape",
		Variants: []variant.Spec{
			{Format: variant.FormatWebP, Quality: 80, Size: variant.Resolution{Width: 480, Height: 480}},
			{Format: variant.FormatWebP, Quality: 80, Size: variant.Resolution{Width: 1200, Height: 1200}},
			{Format: variant.FormatAVIF, Quality: 60, Size: variant.Resolution{Width: 1200, Height: 1200}},
		},
	},
	"avatar": {
		Label:       "Avatar",
		Description: "Square crops at 128 and 256 pixels, in WebP and JPEG",
		Variants: []variant.Spec{
			{Format: variant.FormatWebP, Quality: 80, Size: variant.Resolution{Width: 128, Height: 128}, Fit: variant.FitCover},
			{Format: variant.FormatWebP, Quality: 80, Size: variant.Resolution{Width: 256, Height: 256}, Fit: variant.FitCover},
			{Format: variant.FormatJPEG, Quality: 85, Size: variant.Resolution{Width: 256, Height: 256}, Fit: variant.FitCover},
		},
	},
}

// What each account may upload, hardcoded like the profiles
// A nil list allows every format provino accepts
var uploadRules = map[string]struct {
	AllowedFormats []string
	MaxBytes       int64
}{
	"alice": {MaxBytes: 20 << 20},
	"bob":   {AllowedFormats: []string{"jpg", "png"}, MaxBytes: 2 << 20},
}

// uploadContext is what the web UI sends provino in the upload's metadata, which provino forwards to the authorizer
type uploadContext struct {
	Profile string `json:"profile"`
}

// notifyContext is what the authorizer asks provino to echo back in the job's webhook
type notifyContext struct {
	Owner   string `json:"owner"`
	Profile string `json:"profile"`
}

// exchange is a request provino made to the app, captured so it can be recorded along with the app's answer
type exchange struct {
	receivedAt time.Time
	headers    map[string]string
	body       []byte
	// What the webhook callback did with the event
	outcome string
}

// exchangeKey is the context key of the exchange a request is part of
type exchangeKey struct{}

// responseRecorder keeps a copy of the status and the body the app answers with
type responseRecorder struct {
	http.ResponseWriter

	status int
	body   bytes.Buffer
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b) //nolint:wrapcheck // The error is the client's connection's, which the server handles
}

// Accept only requests that carry a token provino got with its client credentials
func (a *app) requireProvino(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, err := a.oauth.verify(token, a.publicURL, provinoClientID)
		if err != nil {
			a.log.WarnContext(r.Context(), "Refused a request that doesn't come from provino", slog.String("path", r.URL.Path), slog.Any("error", err))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Accept only requests from a signed-in user of the web UI, and pass on the account's ID
func (a *app) requireUser(next func(w http.ResponseWriter, r *http.Request, user string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		user, err := a.oauth.verify(token, a.publicURL, webClientID)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r, user)
	})
}

// Capture a request provino made and the app's answer, and hand them to save once the handler is done
func (a *app) capture(save func(ctx context.Context, x *exchange, status int, response []byte) error) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Read the body first, so it's recorded byte for byte as provino sent it, and hand the handler a copy
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCapturedBody))
			if err != nil {
				http.Error(w, "the request body is too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			x := &exchange{receivedAt: time.Now().UTC(), headers: capturedHeaders(r.Header), body: body}
			rec := &responseRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), exchangeKey{}, x)))

			// The answer is already sent, so failing to record it only costs the web UI an entry
			err = save(r.Context(), x, rec.status, rec.body.Bytes())
			if err != nil {
				a.log.ErrorContext(r.Context(), "Failed to record a request from provino", slog.String("path", r.URL.Path), slog.Any("error", err))
			}
		})
	}
}

// Record a request to the authorizer
func (a *app) saveAuthorization(ctx context.Context, x *exchange, status int, response []byte) error {
	var ar webhook.AuthorizeRequest
	_ = json.Unmarshal(x.body, &ar)
	return a.db.recordAuthorization(ctx, authorizationRecord{
		ReceivedAt:     x.receivedAt,
		Subject:        ar.Subject,
		RequestHeaders: x.headers,
		Request:        x.body,
		Status:         status,
		Response:       bytes.TrimSpace(response),
	})
}

// Record a webhook delivery
func (a *app) saveDelivery(ctx context.Context, x *exchange, status int, _ []byte) error {
	var e webhook.Event
	_ = json.Unmarshal(x.body, &e)
	outcome := x.outcome
	if status >= http.StatusMultipleChoices {
		outcome = "failed"
	}
	return a.db.recordDelivery(ctx, deliveryRecord{
		ReceivedAt:     x.receivedAt,
		WebhookID:      x.headers[webhook.HeaderID],
		Type:           e.Type,
		Namespace:      e.Data.Namespace,
		ImageID:        e.Data.ImageID,
		JobID:          e.Data.JobID,
		Timestamp:      e.Timestamp,
		RequestHeaders: x.headers,
		Body:           x.body,
		Status:         status,
		Outcome:        outcome,
	})
}

// Keep the headers of a request provino made, without the secret part of its token
func capturedHeaders(header http.Header) map[string]string {
	out := make(map[string]string, len(header))
	for name, values := range header {
		value := strings.Join(values, ", ")
		if name == "Authorization" {
			// The token is short-lived and only for this app, but it's still a credential, so only its start is kept
			const shown = len("Bearer ") + 12
			if len(value) > shown {
				value = value[:shown] + "…"
			}
		}
		out[name] = value
	}
	return out
}

// Decide a browser upload: which namespace it goes in, what it may be, which variants to generate, and where to report the job
//
// provino has already verified the user's token, so the subject is the signed-in account, while the context comes from the browser and is checked here
func (a *app) handleAuthorize() http.HandlerFunc {
	return webhook.HandleAuthorize(nil, func(r *http.Request, ar *webhook.AuthorizeRequest) (*webhook.AuthorizeResponse, bool, error) {
		log := a.log.With(slog.String("subject", ar.Subject))

		// Only the accounts this app knows may upload
		rules, known := uploadRules[ar.Subject]
		if !known {
			log.WarnContext(r.Context(), "Refused an upload from an unknown account")
			return nil, false, nil
		}

		// The browser picks one of the profiles, and anything else is refused
		var uc uploadContext
		if len(ar.Context) > 0 {
			err := json.Unmarshal(ar.Context, &uc)
			if err != nil {
				log.WarnContext(r.Context(), "Refused an upload whose context isn't valid", slog.Any("error", err))
				return nil, false, nil
			}
		}
		p, found := profiles[uc.Profile]
		if !found {
			log.WarnContext(r.Context(), "Refused an upload for an unknown profile", slog.String("profile", uc.Profile))
			return nil, false, nil
		}

		// Each account has a namespace of its own, and provino mints the image ID
		notify, err := json.Marshal(notifyContext{Owner: ar.Subject, Profile: uc.Profile})
		if err != nil {
			return nil, false, err //nolint:wrapcheck // HandleAuthorize answers 500 without the error's text
		}
		log.InfoContext(r.Context(), "Authorized an upload", slog.String("profile", uc.Profile))
		return &webhook.AuthorizeResponse{
			Namespace:      ar.Subject,
			MaxBytes:       rules.MaxBytes,
			AllowedFormats: rules.AllowedFormats,
			Variants:       slices.Clone(p.Variants),
			Notify:         &webhook.Notify{Endpoint: notifyEndpoint, Context: notify},
		}, true, nil
	})
}

// Record a job's outcome from provino's webhook
func (a *app) handleEvent() http.HandlerFunc {
	return webhook.HandleEvent(nil, func(r *http.Request, e *webhook.Event) error {
		x, _ := r.Context().Value(exchangeKey{}).(*exchange)
		if x == nil {
			x = &exchange{}
		}
		log := a.log.With(
			slog.String("type", string(e.Type)),
			slog.String("namespace", e.Data.Namespace),
			slog.String("imageId", e.Data.ImageID),
			slog.String("jobId", e.Data.JobID),
		)

		// A newer provino may send event types this app doesn't know, which are acknowledged and ignored
		switch e.Type {
		case webhook.EventImageStored, webhook.EventImageOptimized, webhook.EventImageFailed:
		default:
			x.outcome = outcomeIgnored
			log.InfoContext(r.Context(), "Ignored an event of an unknown type")
			return nil
		}

		var nc notifyContext
		if len(e.Data.Context) > 0 {
			_ = json.Unmarshal(e.Data.Context, &nc)
		}

		// Every delivery attempt carries the same webhook ID, which is how a repeat is recognized
		outcome, err := a.db.applyEvent(r.Context(), r.Header.Get(webhook.HeaderID), e, nc)
		if err != nil {
			log.ErrorContext(r.Context(), "Failed to record an event", slog.Any("error", err))
			return err
		}
		x.outcome = outcome
		log.InfoContext(r.Context(), "Received an event", slog.String("outcome", outcome), slog.String("error", e.Data.Error))
		return nil
	})
}

// List the signed-in user's images, newest first
func (a *app) handleImages(w http.ResponseWriter, r *http.Request, user string) {
	images, err := a.db.images(r.Context(), user)
	if err != nil {
		a.log.ErrorContext(r.Context(), "Failed to list the images", slog.Any("error", err))
		http.Error(w, "failed to list the images", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, images)
}

// List the requests provino made to the authorizer for the signed-in user's uploads, newest first
func (a *app) handleAuthorizations(w http.ResponseWriter, r *http.Request, user string) {
	list, err := a.db.authorizations(r.Context(), user)
	if err != nil {
		a.log.ErrorContext(r.Context(), "Failed to list the authorization requests", slog.Any("error", err))
		http.Error(w, "failed to list the authorization requests", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// List the webhooks provino delivered about the signed-in user's images, newest first
func (a *app) handleDeliveries(w http.ResponseWriter, r *http.Request, user string) {
	list, err := a.db.deliveries(r.Context(), user)
	if err != nil {
		a.log.ErrorContext(r.Context(), "Failed to list the deliveries", slog.Any("error", err))
		http.Error(w, "failed to list the deliveries", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
