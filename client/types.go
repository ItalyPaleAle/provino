package client

import (
	"encoding/json"
	"time"

	"github.com/italypaleale/provino/variant"
	"github.com/italypaleale/provino/webhook"
)

// IngestMetadata is the `metadata` part of a browser upload to `POST /v1/public/ingest`
type IngestMetadata struct {
	// Opaque JSON object forwarded verbatim to the authorizer, at most 4 KB
	Context json.RawMessage `json:"context,omitempty"`
}

// UploadMetadata is the `metadata` part of `POST /v1/client/upload`, which must come before the image
type UploadMetadata struct {
	// Namespace the image is stored in
	Namespace string `json:"namespace"`
	// ID of the image, a canonical UUID that isn't in use yet
	// If empty, provino mints one
	ImageID string `json:"imageId,omitempty"`
	// Variants to generate once the upload is stored, which can be none
	Variants []variant.Spec `json:"variants,omitempty"`
	// Where the job reports its outcome
	Notify *webhook.Notify `json:"notify,omitempty"`
}

// OptimizeRequest is the body of `POST /v1/client/optimize`
type OptimizeRequest struct {
	// Namespace and ID of an existing image
	Namespace string `json:"namespace"`
	ImageID   string `json:"imageId"`
	// Every variant the image should have, including the ones it already has
	// Stored files that still match are reused, so sending the whole set costs nothing for the variants that exist
	Variants []variant.Spec `json:"variants"`
	// Where the job reports its outcome
	Notify *webhook.Notify `json:"notify,omitempty"`
	// Format of the original, which is only needed for an image provino has no manifest for: `jpg`, `png`, `webp`, `avif`, `heic`, `heif`
	SourceFormat string `json:"sourceFormat,omitempty"`
}

// JobState is where a job is in its life
type JobState string

const (
	// JobRunning means the job is generating variants
	JobRunning JobState = "running"
	// JobCompleted means the job finished and the image's manifest describes its result
	JobCompleted JobState = "completed"
	// JobFailed means the job failed and its effects were undone
	JobFailed JobState = "failed"
)

// JobAccepted is the answer to a request that started a job
type JobAccepted struct {
	// Namespace and ID of the image the job works on
	Namespace string `json:"namespace"`
	ImageID   string `json:"imageId"`
	// ID of the job, a UUIDv7
	JobID string `json:"jobId"`
	// Always `running`: the job's workflow has started, and it does its work after the response is sent
	Status JobState `json:"status"`
	// Key of the original in the originals store, set for uploads
	SourceKey string `json:"sourceKey,omitempty"`
	// Key of the image's manifest in the variants store
	ManifestKey string `json:"manifestKey"`
}

// JobStatus describes a job
type JobStatus struct {
	JobID     string   `json:"jobId"`
	Namespace string   `json:"namespace"`
	ImageID   string   `json:"imageId"`
	Status    JobState `json:"status"`
	// Number of variants the job generates, and how many are done
	Total     int `json:"total"`
	Completed int `json:"completed"`
	// Why the job failed; it starts with `image_not_found` when the image's original doesn't exist
	Error       string    `json:"error,omitempty"`
	RequestedAt time.Time `json:"requestedAt,omitzero"`
	StartedAt   time.Time `json:"startedAt,omitzero"`
	CompletedAt time.Time `json:"completedAt,omitzero"`
}

// Error is the body of every error response
type Error struct {
	// Machine-readable code, for example `image_busy`
	Code string `json:"code"`
	// Human-readable description
	Message string `json:"message"`
	// Details about what was wrong with the request, when there are any
	InnerError string `json:"innerError,omitempty"`
	// Extra values some errors carry
	Metadata map[string]string `json:"metadata,omitempty"`
}
