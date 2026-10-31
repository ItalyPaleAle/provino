// Package imagekeys is the object key layout provino stores an image's objects under
//
// Applications that serve the variants, or that read the original back, address the same objects in the same stores, so the layout is a contract rather than an implementation detail
// A variant's key ends with a random file ID, so readers find the variants through the manifest, which records the full key of every object it describes
package imagekeys

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"uuid"

	"github.com/italypaleale/provino/variant"
)

// Name of the object describing what a job produced for one image
const manifestName = "manifest.json"

// A namespace is a short, lowercase, path-safe name, so it can be the first segment of every key without escaping it
var namespaceRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Number of random bytes in a file ID, which is written as twice as many hex digits
const fileIDBytes = 4

// A file ID is the random part of a variant's key, in lowercase hex
var fileIDRegex = regexp.MustCompile(`^[0-9a-f]{8}$`)

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

// AssetKey returns the key of one generated variant of an image, for example "<imageId>_1200x800_3f9a0c1e.webp"
//
// The size is the one the file came out at, so the name tells what's in it, and the file ID is random, so every file a job encodes is a new object
// A key is written once and its bytes never change, which is what lets an application serve it as immutable and lets a failed job delete every file it wrote
// The file ID must have been made by NewFileID or checked with ValidateFileID
func (l Layout) AssetKey(namespace string, imageID uuid.UUID, width int, height int, fileID string, format variant.Format) string {
	return l.prefix(namespace, imageID) + imageID.String() + "_" + strconv.Itoa(width) + "x" + strconv.Itoa(height) + "_" + fileID + format.Extension()
}

// OwnsKey reports whether a key names an object of one image: one stored under the image's prefix, with a name that starts with the image's ID
// Keys read back from a manifest are checked with it, so a manifest can only ever point a job at the files of its own image
func (l Layout) OwnsKey(namespace string, imageID uuid.UUID, key string) bool {
	name, found := strings.CutPrefix(key, l.prefix(namespace, imageID)+imageID.String()+"_")
	return found && name != "" && !strings.Contains(name, "/") && !strings.Contains(name, "..")
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

// NewFileID returns a random file ID, the last part of a variant's key, as 8 lowercase hex digits
// It only has to be unique among the files of one image, which the caller checks against the IDs the image already uses
func NewFileID() string {
	b := make([]byte, fileIDBytes)
	// crypto/rand never returns an error
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ValidateFileID checks that a file ID read back from durable state is one NewFileID could have made, so it's safe in an object key
func ValidateFileID(fileID string) error {
	if !fileIDRegex.MatchString(fileID) {
		return fmt.Errorf("file ID '%s' is not 8 lowercase hex digits", fileID)
	}
	return nil
}

// FileIDOf returns the file ID a variant's key ends with, or an empty string for a key that doesn't end with one, such as a name an older layout gave a file
func FileIDOf(key string) string {
	name := key[strings.LastIndexByte(key, '/')+1:]
	dot := strings.LastIndexByte(name, '.')
	if dot >= 0 {
		name = name[:dot]
	}
	fileID := name[strings.LastIndexByte(name, '_')+1:]
	if !fileIDRegex.MatchString(fileID) {
		return ""
	}
	return fileID
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
