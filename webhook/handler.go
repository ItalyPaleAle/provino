package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// maxBodyBytes bounds the body the handlers read, which is room for a manifest with thousands of variants
const maxBodyBytes = 1 << 20

// HandleAuthorize returns a handler for the authorizer endpoint, which verifies the request, decodes provino's AuthorizeRequest, and answers with the callback's decision
// The request is verified with v, and answered 401 when no signature matches; v can be nil when provino sends an OAuth2 bearer token, which a middleware in front of the handler checks instead
// The callback gets the request for its context and headers, with the body already read
// It returns the response and true to allow the upload, or false to refuse it, which answers 403
// An error, or true with a nil response, answers 500, which provino treats as a failure of the authorizer; the error isn't sent, so the callback should log it
func HandleAuthorize(v *Verifier, fn func(r *http.Request, ar *AuthorizeRequest) (*AuthorizeResponse, bool, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req AuthorizeRequest
		status, err := readBody(w, r, v, &req)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}

		// A request in another version of the contract may mean something this code doesn't know about, so it's refused rather than answered
		if req.Version != AuthorizeVersion {
			http.Error(w, fmt.Sprintf("unsupported contract version '%s'", req.Version), http.StatusBadRequest)
			return
		}

		res, allowed, err := fn(r, &req)
		switch {
		case err != nil:
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		case !allowed:
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		case res == nil:
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		// The response is encoded before anything is written, so an encoding failure can still answer 500
		body, err := json.Marshal(res)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// HandleEvent returns a handler for a webhook endpoint, which verifies the request, decodes the Event provino delivers, and passes it to the callback
// The request is verified with v, and answered 401 when no signature matches; v can be nil when provino sends an OAuth2 bearer token, which a middleware in front of the handler checks instead
// The callback gets the request for its context and headers, with the body already read; its webhook-id header stays the same across attempts, so it identifies a repeated delivery
// The event is acknowledged with a 204 when the callback returns nil, and answered 500 when it returns an error, which makes provino retry the delivery; the error isn't sent, so the callback should log it
// Deliveries are at least once, so the callback must be idempotent, and it should accept event types it doesn't know, because a newer provino may send more
func HandleEvent(v *Verifier, fn func(r *http.Request, e *Event) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var event Event
		status, err := readBody(w, r, v, &event)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}

		err = fn(r, &event)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// Read a request's body, verify it when there's a verifier, and decode it as JSON, returning the status to answer with when any of it fails
func readBody(w http.ResponseWriter, r *http.Request, v *Verifier, dst any) (int, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		_, tooLarge := errors.AsType[*http.MaxBytesError](err)
		if tooLarge {
			return http.StatusRequestEntityTooLarge, fmt.Errorf("the request body is larger than %d bytes", maxBodyBytes)
		}
		return http.StatusBadRequest, errors.New("failed to read the request body")
	}

	// The signature covers the exact bytes received, so it's checked before anything is decoded
	if v != nil {
		err = v.Verify(r.Context(), r.Header, body)
		if err != nil {
			return http.StatusUnauthorized, errors.New(http.StatusText(http.StatusUnauthorized))
		}
	}

	err = json.Unmarshal(body, dst)
	if err != nil {
		return http.StatusBadRequest, errors.New("failed to decode the request body as JSON")
	}

	return http.StatusOK, nil
}
