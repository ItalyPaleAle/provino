package webhook

import (
	"encoding/json"

	"github.com/italypaleale/provino/variant"
)

// AuthorizeVersion is the version of the authorizer contract, sent in every AuthorizeRequest
const AuthorizeVersion = "provino/v1"

// AuthorizeRequest is the body provino sends to the authorizer before it accepts a browser upload
//
// The authorizer answers 200 with an AuthorizeResponse to allow the upload, or 403 to refuse it
// Any other answer, a 404 included, is treated as a failure of the authorizer, and the upload is refused with a 502
type AuthorizeRequest struct {
	// Version of the contract, always AuthorizeVersion
	Version string `json:"version"`
	// Subject of the token the browser presented
	Subject string `json:"subject"`
	// The opaque context the browser sent in the upload's metadata part, forwarded verbatim
	Context json.RawMessage `json:"context,omitempty"`
	// The file being uploaded, as the browser described it
	File AuthorizeFile `json:"file"`
}

// AuthorizeFile is what the browser said about the file it is uploading
// Neither value is trusted: provino detects the format from the file's content
type AuthorizeFile struct {
	Name        string `json:"name,omitempty"`
	ContentType string `json:"contentType,omitempty"`
}

// AuthorizeResponse is the authorizer's decision about an upload it allows
type AuthorizeResponse struct {
	// Namespace the image is stored in
	// It must be 1 to 64 lowercase letters, digits, dots, underscores or dashes, starting with a letter or digit
	Namespace string `json:"namespace"`
	// ID of the image, which must be a canonical UUID that isn't in use yet: a v4, or a v7 for an image that already has one
	// If empty, provino mints one
	ImageID string `json:"imageId,omitempty"`
	// Maximum size of the upload in bytes, applied together with provino's own limit
	// If zero, only provino's limit applies
	MaxBytes int64 `json:"maxBytes,omitempty"`
	// Source formats the upload may be in: `jpg`, `png`, `webp`, `avif`, `heic`, `heif`
	// If empty, every format provino supports is allowed
	AllowedFormats []string `json:"allowedFormats,omitempty"`
	// Variants to generate once the upload is stored
	Variants []variant.Spec `json:"variants,omitempty"`
	// Where the job reports its outcome
	Notify *Notify `json:"notify,omitempty"`
}
