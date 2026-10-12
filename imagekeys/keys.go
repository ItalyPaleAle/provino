// Package imagekeys is the object key layout provino stores an image's objects under
//
// Applications that serve the variants, or that read the original back, address the same objects in the same stores, so the layout is a contract rather than an implementation detail
// The manifest records the full key of every object it describes, so most readers don't need this package; it's here for the ones that compute keys themselves
package imagekeys

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"uuid"
)

// Name of the object describing what a job produced for one image
const manifestName = "manifest.json"

// A namespace is a short, lowercase, path-safe name, so it can be the first segment of every key without escaping it
var namespaceRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Layout builds the keys of the objects belonging to one image
//
// Every object is written under two folder levels taken from its image ID, so no single folder holds every image of a namespace
// The layout does not vary by backend: an object store treats a key as opaque and would be happy with one folder per namespace, but a namespace accumulating thousands of images makes that folder unusable to browse, list or reason about, and a local file system degrades outright
//
// The namespace passed to every method must have been checked with ValidateNamespace
type Layout struct{}

// New creates a Layout
func New() Layout {
	return Layout{}
}

// OriginalKey returns the key of the source image that was uploaded
func (l Layout) OriginalKey(namespace string, imageID uuid.UUID, format string) string {
	return l.prefix(namespace, imageID) + imageID.String() + "_orig" + extension(format)
}

// AssetKey returns the key of one generated variant of an image
//
// The quality a variant was encoded at is deliberately absent: an image has one variant per size and format, and the quality it is encoded at is a property of the request that created it rather than of the object
// Putting it in the key would make two encodings of the same size and format two different objects, which is not something anything downstream can address
func (l Layout) AssetKey(namespace string, imageID uuid.UUID, width int, height int, format string) string {
	name := imageID.String() + "_" + strconv.Itoa(width) + "x" + strconv.Itoa(height) + extension(format)
	return l.prefix(namespace, imageID) + name
}

// ManifestKey returns the key of the manifest describing everything generated for an image
func (l Layout) ManifestKey(namespace string, imageID uuid.UUID) string {
	return l.prefix(namespace, imageID) + imageID.String() + "_" + manifestName
}

// Prefix returns the folder every object of one image is written under
// It is what a caller lists to find them, and what it deletes to remove them
func (l Layout) Prefix(namespace string, imageID uuid.UUID) string {
	return l.prefix(namespace, imageID)
}

func (l Layout) prefix(namespace string, imageID uuid.UUID) string {
	return namespace + "/" + shard(imageID) + "/"
}

// Split an image ID into two folder levels, so no single directory holds every newly created image of a namespace
//
// New image IDs are v4 so their leading characters spread evenly across the 256 folders
// Images that kept a v7 ID from before keep their existing identity and object keys even though their timestamp prefix provides less fan-out
func shard(imageID uuid.UUID) string {
	s := imageID.String()
	return s[:2] + "/" + s[2:4]
}

// ValidateNamespace checks that a namespace can be the first segment of an object key
// It is 1 to 64 characters long, starts with a lowercase letter or digit, and contains only lowercase letters, digits, dots, underscores and dashes, never two dots in a row
func ValidateNamespace(namespace string) error {
	if !namespaceRegex.MatchString(namespace) || strings.Contains(namespace, "..") {
		return fmt.Errorf("namespace '%s' is not valid: it must be 1 to 64 lowercase letters, digits, dots, underscores or dashes, starting with a letter or digit", namespace)
	}
	return nil
}

// ValidateID parses an identifier that arrived from outside the process and checks it is a canonical UUIDv4 (recommended) or UUIDv7
//
// Newly minted image identifiers use v4 so object keys retain even shard distribution
func ValidateID(id string) (uuid.UUID, error) {
	parsed, err := parseCanonicalID(id)
	if err != nil {
		return uuid.Nil(), err
	}

	version := parsed[6] >> 4
	if version != 4 && version != 7 {
		return uuid.Nil(), fmt.Errorf("identifier '%s' is not a UUIDv4 or UUIDv7", id)
	}

	return parsed, nil
}

// Parsing alone is not enough because only canonical text maps an identity back to one stable object key
func parseCanonicalID(id string) (uuid.UUID, error) {
	if id == "" {
		return uuid.Nil(), errors.New("identifier is empty")
	}

	parsed, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("identifier '%s' is not a UUID: %w", id, err)
	}
	if parsed.String() != id {
		return uuid.Nil(), fmt.Errorf("identifier '%s' is not in canonical form", id)
	}

	if parsed[8]&0xc0 != 0x80 {
		return uuid.Nil(), fmt.Errorf("identifier '%s' is not an RFC 9562 UUID", id)
	}

	return parsed, nil
}

// Normalize a format into the extension it is stored with, always starting with a dot
func extension(format string) string {
	format = strings.ToLower(strings.TrimSpace(format))
	format = strings.TrimPrefix(format, ".")
	if format == "" {
		return ""
	}

	return "." + format
}
