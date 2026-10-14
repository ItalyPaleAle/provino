// Package webhook holds what an application needs to receive calls from provino: the events a job sends when it finishes, the authorizer contract provino calls before accepting a browser upload, and a verifier for both
//
// Both directions follow the Standard Webhooks specification (https://www.standardwebhooks.com): a request carries the webhook-id, webhook-timestamp and webhook-signature headers, and the signature covers "<webhook-id>.<webhook-timestamp>.<body>"
package webhook

import (
	"encoding/json"
	"time"

	"github.com/italypaleale/provino/manifest"
)

// EventType names what happened to an image
type EventType string

const (
	// EventImageStored is sent when a job that asked for no variants completes, so the original is stored and recorded in its manifest
	EventImageStored EventType = "image.stored"
	// EventImageOptimized is sent when a job that asked for variants completes, so every variant is stored and listed in the manifest
	EventImageOptimized EventType = "image.optimized"
	// EventImageFailed is sent when a job fails, after its effects have been undone and the previous manifest put back
	EventImageFailed EventType = "image.failed"
)

// Event is the body of a webhook request provino sends
type Event struct {
	// What happened
	Type EventType `json:"type"`
	// When the event was produced, which stays the same across delivery attempts
	Timestamp time.Time `json:"timestamp"`
	// Details of the event
	Data EventData `json:"data"`
}

// EventData describes the job an event is about
type EventData struct {
	// Namespace and ID of the image
	Namespace string `json:"namespace"`
	ImageID   string `json:"imageId"`
	// ID of the job that sent the event
	JobID string `json:"jobId"`
	// Key of the image's manifest in the variants store
	ManifestKey string `json:"manifestKey"`
	// The manifest the job wrote, or the one it left in place when it failed, if there's one
	Manifest *manifest.Manifest `json:"manifest,omitempty"`
	// Why the job failed, set on image.failed only
	Error string `json:"error,omitempty"`
	// The context the request that started the job attached to its notification, echoed back verbatim
	Context json.RawMessage `json:"context,omitempty"`
}

// Notify asks a job to report its outcome to one of the webhook endpoints configured in provino
type Notify struct {
	// Name of the endpoint in provino's configuration, or empty for the default endpoint
	Endpoint string `json:"endpoint,omitempty"`
	// Opaque JSON object that is echoed back in the event, at most 4 KB
	Context json.RawMessage `json:"context,omitempty"`
}
