// Package manifest is the document provino writes next to an image's variants, describing the original and every variant that was generated from it
//
// The manifest is stored at the image's manifest key in the variants store, and it's also the payload of the webhook events a job sends
// Every object it describes carries its full key, so a reader can fetch the files without computing the storage layout itself
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/italypaleale/provino/variant"
)

// Version identifies the format of a manifest
// Readers check it before trusting anything else in the document, so a future format can change the rest freely
const Version = "provino/v1"

// Status says what a manifest describes
type Status string

const (
	// StatusStored means the original is stored and no job has generated variants for it yet
	StatusStored Status = "stored"
	// StatusCompleted means every variant the last successful job asked for is available
	// A job that fails leaves the manifest it found in place, so no manifest describes a failed job
	StatusCompleted Status = "completed"
)

// Manifest describes an image: its original, and the variants generated from it
type Manifest struct {
	// Format of this document, always Version
	Version string `json:"version"`
	// Namespace the image belongs to, which is the first segment of every key below
	Namespace string `json:"namespace"`
	// ID of the image
	ImageID string `json:"imageId"`
	// What the manifest describes
	Status Status `json:"status"`
	// ID of the job that wrote this manifest, empty for the one written when the original was stored
	JobID string `json:"jobId,omitempty"`

	// Key of the original in the originals store
	SourceKey string `json:"sourceKey"`
	// Format of the original, detected from its content: `jpg`, `png`, `webp`, `avif`, `heic`, or `heif`
	SourceFormat string `json:"sourceFormat"`
	// Size of the original, in bytes
	SourceSize int64 `json:"sourceSize,omitempty"`
	// Width and height of the original after its EXIF orientation is applied, which decide the boxes it can fill
	SourceWidth  int `json:"sourceWidth,omitempty"`
	SourceHeight int `json:"sourceHeight,omitempty"`

	// When the original was stored
	StoredAt time.Time `json:"storedAt,omitzero"`
	// When the job that wrote this manifest was requested, started, and completed
	RequestedAt time.Time `json:"requestedAt,omitzero"`
	StartedAt   time.Time `json:"startedAt,omitzero"`
	CompletedAt time.Time `json:"completedAt,omitzero"`

	// Every variant the job asked for, in the order it asked for them, each of them available
	Variants []Variant `json:"variants"`
}

// Variant describes one variant in the manifest
type Variant struct {
	// Key of the file in the variants store
	// A skipped entry repeats the key of the entry that holds its pixels
	Key string `json:"key"`
	// Name the variant is listed under, unique within the manifest
	Name string `json:"name"`
	// Format the file is encoded in
	Format variant.Format `json:"format"`
	// Encoder settings the file was produced with
	Quality int `json:"quality,omitempty"`
	Effort  int `json:"effort,omitempty"`
	// Box the variant was requested for
	Resolution variant.Resolution `json:"requestedResolution"`
	// How the source was mapped onto the box
	Fit variant.Fit `json:"fit"`
	// Actual size of the file, which can be smaller than the box
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// Size of the file, in bytes
	Size int64 `json:"size,omitempty"`
	// Skipped marks a box the source can't fill whose pixels a tighter box of the same fit and format already holds, so no file was stored for it
	// The entry repeats that file's key, width, height and size, so a reader that doesn't know about skipped entries sees a duplicate of that file and drops it
	Skipped bool `json:"skipped,omitempty"`
	// SkippedFor is the box whose entry holds the file, set on skipped entries only
	SkippedFor *variant.Resolution `json:"skippedFor,omitempty"`
}

// ErrUnsupportedVersion is returned by Parse for a document that isn't a provino/v1 manifest
var ErrUnsupportedVersion = errors.New("the document is not a " + Version + " manifest")

// Parse decodes a manifest, refusing any document that doesn't declare Version
func Parse(data []byte) (Manifest, error) {
	// The version is read on its own first, so a document in another format is reported as such rather than as whatever field fails to decode
	var header struct {
		Version string `json:"version"`
	}
	err := json.Unmarshal(data, &header)
	if err != nil {
		return Manifest{}, fmt.Errorf("failed to decode the manifest: %w", err)
	}
	if header.Version != Version {
		return Manifest{}, fmt.Errorf("%w: it declares version '%s'", ErrUnsupportedVersion, header.Version)
	}

	var m Manifest
	err = json.Unmarshal(data, &m)
	if err != nil {
		return Manifest{}, fmt.Errorf("failed to decode the manifest: %w", err)
	}

	return m, nil
}
