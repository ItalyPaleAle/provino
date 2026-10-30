package actors

import (
	"encoding/json"
	"time"

	"github.com/italypaleale/provino/internal/imaging"
	"github.com/italypaleale/provino/manifest"
	"github.com/italypaleale/provino/webhook"
)

// Names in the Francis definitions
const (
	jobWorkflowName    = "provino-job"
	jobWorkflowVersion = 1

	imageActorType = "provino-image"

	stepSnapshot = "snapshot"
	stepPrepare  = "prepare"
	stepGenerate = "generate"
	stepManifest = "manifest"
	stepNotify   = "notify"
	stepCleanup  = "cleanup"
	stepRelease  = "release"
)

// ErrorImageNotFound starts the error of a job whose original doesn't exist, so a receiver can tell it apart from a failure worth retrying
const ErrorImageNotFound = "image_not_found"

// Status of a job
type Status string

const (
	// StatusRunning means the job's workflow has started
	StatusRunning Status = "running"
	// StatusCompleted means the job finished and its manifest is stored
	StatusCompleted Status = "completed"
	// StatusFailed means the job failed and its effects were undone
	StatusFailed Status = "failed"
)

// VariantSpec describes one variant a job has to generate, in the form the workflow carries it
type VariantSpec struct {
	// Position of the variant within the job, which identifies it in the fan-out
	Index int `json:"index"`
	// Name the variant is listed under in the manifest
	Name string `json:"name"`
	// Format the variant is encoded to
	Format imaging.Format `json:"format"`
	// Encoder quality, for lossy formats
	Quality int `json:"quality,omitempty"`
	// Encoder CPU effort, for AVIF and WebP
	Effort int `json:"effort,omitempty"`
	// Requested resolution
	Resolution imaging.Resolution `json:"resolution"`
	// How the source image is mapped onto this variant's resolution
	Fit imaging.Fit `json:"fit"`
}

// JobSource is what the upload handler learned about the original it stored
type JobSource struct {
	// Format of the original, detected from its content, without a leading dot
	Format string `json:"format"`
	// Size of the original in bytes
	Size int64 `json:"size"`
	// Size of the original after its EXIF orientation
	Width  int `json:"width"`
	Height int `json:"height"`
	// When the original was stored
	StoredAt time.Time `json:"storedAt"`
}

// JobRequest is the durable input of a job
type JobRequest struct {
	// ID of the job, minted by the image's actor
	JobID string `json:"jobId"`
	// Image the job works on
	Namespace string `json:"namespace"`
	ImageID   string `json:"imageId"`
	// What the upload handler learned about the original, set on jobs created by an upload
	Source *JobSource `json:"source,omitempty"`
	// Format of the original, used only when the image has no manifest to read it from
	SourceFormat string `json:"sourceFormat,omitempty"`
	// Variants to generate, which can be none
	Variants []VariantSpec `json:"variants"`
	// Where the job reports its outcome, with the endpoint already resolved, or nil
	Notify *webhook.Notify `json:"notify,omitempty"`
	// SHA-256 of the subject that started the job, which is what lets a browser read the status of its own jobs and nobody else's
	SubjectHash string `json:"subjectHash,omitempty"`
	// When the job was requested
	RequestedAt time.Time `json:"requestedAt"`
}

// jobSnapshot is what the snapshot step found before the job changed anything
type jobSnapshot struct {
	// The manifest that was stored, byte for byte, so a failed job can put back exactly what it found
	// It's empty when the image had no manifest
	Previous json.RawMessage `json:"previous,omitempty"`
	// Format of the original, without a leading dot
	SourceFormat string `json:"sourceFormat"`
	// Size of the original in bytes, when it's known
	SourceSize int64 `json:"sourceSize,omitempty"`
	// Size of the original after its EXIF orientation, when a manifest or the upload recorded it
	SourceResolution imaging.Resolution `json:"sourceResolution,omitzero"`
	// When the original was stored, when it's known
	StoredAt time.Time `json:"storedAt,omitzero"`
	// When the job started, shared by every step so the manifest doesn't depend on mutable state
	StartedAt time.Time `json:"startedAt"`
}

// variantTask is one item in the generation fan-out
type variantTask struct {
	// Image the variant belongs to
	Namespace string `json:"namespace"`
	ImageID   string `json:"imageId"`
	// Format of the source image to read
	SourceFormat string `json:"sourceFormat"`
	// The variant to generate
	Spec VariantSpec `json:"spec"`
	// Existing is the stored manifest entry reused instead of encoding this resolution and format again
	Existing *manifest.Variant `json:"existing,omitempty"`
	// Skip is set when the source can't fill this box and another variant of the job already holds the same pixels, so nothing is encoded
	Skip *SkippedVariant `json:"skip,omitempty"`
	// SourceSize is the source's size after its EXIF orientation, which every item carries so the manifest can record it from durable outputs
	SourceSize imaging.Resolution `json:"sourceSize,omitzero"`
	// Reserved lists the stored files other variants of the job reuse, set on the items that encode
	// An encode that comes out at the size and format of one of them must not replace it with different encoder settings, since the manifest describes the stored bytes
	Reserved []reservedFile `json:"reserved,omitempty"`
}

// reservedFile is a stored file that a variant of the job reuses
type reservedFile struct {
	// Name of the variant that reuses the file
	Name string `json:"name"`
	// The file's entry in the manifest the job found
	Stored manifest.Variant `json:"stored"`
}

// Find the reserved file an encode of the given size and format would be stored as
func (t variantTask) reservedAt(width int, height int, format imaging.Format) (reservedFile, bool) {
	for _, r := range t.Reserved {
		if r.Stored.Width == width && r.Stored.Height == height && r.Stored.Format.Extension() == format.Extension() {
			return r, true
		}
	}
	return reservedFile{}, false
}

// SkippedVariant points a box the source can't fill at the variant of the same job that holds its pixels
// Every box the source can't fill gets the source's own pixels, so for each fit and format only the tightest of them stores a file
type SkippedVariant struct {
	// Position of the variant that holds the file
	Index int `json:"index"`
	// Box that variant requested, which the manifest reports as skippedFor
	For imaging.Resolution `json:"for"`
}

// VariantResult is the output of one item in the generation fan-out
// The fan-out fails as soon as one variant does, so a result is only ever recorded for a variant that's available
type VariantResult struct {
	// Index of the variant within the job
	Index int `json:"index"`
	// Name of the variant
	Name string `json:"name"`
	// Size of the encoded variant, in bytes
	Size int64 `json:"size,omitempty"`
	// Actual width of the variant, which can be smaller than requested when fitting inside the requested box
	Width int `json:"width,omitempty"`
	// Actual height of the variant
	Height int `json:"height,omitempty"`
	// Time it took to generate the variant, in milliseconds
	DurationMs int64 `json:"durationMs,omitempty"`
	// When the variant was completed
	CompletedAt time.Time `json:"completedAt"`
	// Reused preserves the metadata of an existing stored object that was not regenerated
	Reused *manifest.Variant `json:"reused,omitempty"`
	// Skipped is set when nothing was encoded because another variant holds this box's pixels
	Skipped *SkippedVariant `json:"skipped,omitempty"`
}

// JobStatus is the read-only view of a job's progress
type JobStatus struct {
	JobID     string `json:"jobId"`
	Namespace string `json:"namespace"`
	ImageID   string `json:"imageId"`
	Status    Status `json:"status"`
	// Number of variants the job generates, and how many are done
	Total     int `json:"total"`
	Completed int `json:"completed"`
	// Why the job failed
	Error       string    `json:"error,omitempty"`
	RequestedAt time.Time `json:"requestedAt,omitzero"`
	StartedAt   time.Time `json:"startedAt,omitzero"`
	CompletedAt time.Time `json:"completedAt,omitzero"`
}
