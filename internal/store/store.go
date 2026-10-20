// Package store persists the source images the service receives and the variants its jobs generate
//
// Both live in an object store selected by a DSN, which is [objstore], and they are addressed by the layout in [imagekeys]
// Applications read the same objects out of the same stores, so this package chooses where nothing goes: it only decides what to write and when
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/italypaleale/go-kit/objstore"

	"github.com/italypaleale/provino/imagekeys"
	"github.com/italypaleale/provino/internal/sniff"
)

const (
	// Content type of the manifest, which is the only object the service writes that isn't an image
	manifestContentType = "application/json"
	// Timeout for the deletes that clean up after a failed write, which run on a context of their own
	cleanupTimeout = 10 * time.Second
)

// The object store reports these, and they're re-exported so the rest of the service doesn't have to import it to tell one failure from another
var (
	// ErrTooLarge is returned when an object exceeds the maximum size the caller allowed
	ErrTooLarge = objstore.ErrTooLarge

	// ErrNotFound is returned when an object key doesn't exist in the backend
	ErrNotFound = objstore.ErrNotFound
)

// Store is the pair of backends the service uses: one for the images it receives, one for the variants it generates
// They're separate because the two have different lifetimes and often different visibility: the variants are usually served to browsers, while the originals are not
type Store struct {
	originals objstore.Backend
	variants  objstore.Backend
	keys      imagekeys.Layout
}

// NewOpts contains the options for New
type NewOpts struct {
	// DSN of the backend that stores the uploaded source images
	OriginalsDSN string
	// DSN of the backend that stores the generated variants and the manifests
	VariantsDSN string
}

// New creates a Store from the DSN of each of its two backends
func New(opts NewOpts) (*Store, error) {
	originals, err := newBackend(opts.OriginalsDSN)
	if err != nil {
		return nil, fmt.Errorf("failed to create the storage backend for the originals: %w", err)
	}

	variants, err := newBackend(opts.VariantsDSN)
	if err != nil {
		return nil, fmt.Errorf("failed to create the storage backend for the variants: %w", err)
	}

	// The layout is the same whatever the DSNs selected, because applications address these objects in these same stores and the service can't pick a shape of its own
	return &Store{
		originals: originals,
		variants:  variants,
		keys:      imagekeys.New(),
	}, nil
}

// OriginalsDescription returns a description of the backend the source images are stored in, for logging
func (s *Store) OriginalsDescription() string {
	return s.originals.Description()
}

// VariantsDescription returns a description of the backend the variants are stored in, for logging
func (s *Store) VariantsDescription() string {
	return s.variants.Description()
}

// OriginalKey returns the key of an image's source
func (s *Store) OriginalKey(namespace string, imageID uuid.UUID, format string) string {
	return s.keys.OriginalKey(namespace, imageID, format)
}

// AssetKey returns the key of one generated variant
func (s *Store) AssetKey(namespace string, imageID uuid.UUID, width int, height int, format string) string {
	return s.keys.AssetKey(namespace, imageID, width, height, format)
}

// ManifestKey returns the key of the manifest describing an image
func (s *Store) ManifestKey(namespace string, imageID uuid.UUID) string {
	return s.keys.ManifestKey(namespace, imageID)
}

// SaveOriginal writes an image's source and returns the key it was written to
//
// A write replaces whatever is stored at the key, so a caller that must not overwrite an image checks ImageExists first, while holding the image's upload claim so no other upload can slip in between
// Reading stops with ErrTooLarge as soon as more than maxSize bytes are read, so an oversized upload is never fully buffered or stored
func (s *Store) SaveOriginal(ctx context.Context, namespace string, imageID uuid.UUID, format string, r io.Reader, maxSize int64, contentType string) (key string, size int64, err error) {
	key = s.OriginalKey(namespace, imageID, format)

	result, err := s.originals.Put(ctx, key, r, maxSize, contentType)
	if err != nil {
		// A partially-written object is removed on every failure path, so a rejected request doesn't leave an unusable source image behind
		s.removeOriginalQuietly(ctx, key)
		return "", 0, fmt.Errorf("failed to store the source image '%s': %w", key, err)
	}
	size = result.Size
	if size == 0 {
		s.removeOriginalQuietly(ctx, key)
		return "", 0, errors.New("uploaded image is empty")
	}

	return key, size, nil
}

// OriginalExists reports whether an image's source is stored
func (s *Store) OriginalExists(ctx context.Context, namespace string, imageID uuid.UUID, format string) (bool, error) {
	return s.originalExists(ctx, s.OriginalKey(namespace, imageID, format))
}

// ImageExists reports whether anything is stored for an image: its manifest, or an original in any of the source formats
//
// The key of an original carries its format, so looking only for the format of a new upload would let the same ID be stored again in another one
// The lookups are metadata requests that run together, since on an object store each one is a round trip
func (s *Store) ImageExists(ctx context.Context, namespace string, imageID uuid.UUID) (bool, error) {
	lookups := make([]func() (bool, error), 0, len(sniff.Formats)+1)
	lookups = append(lookups, func() (bool, error) {
		key := s.ManifestKey(namespace, imageID)
		exists, err := s.variants.Exists(ctx, key)
		if err != nil {
			return false, fmt.Errorf("failed to look up the manifest '%s': %w", key, err)
		}
		return exists, nil
	})
	for _, format := range sniff.Formats {
		lookups = append(lookups, func() (bool, error) {
			return s.OriginalExists(ctx, namespace, imageID, format)
		})
	}

	found := make([]bool, len(lookups))
	errs := make([]error, len(lookups))
	var wg sync.WaitGroup
	for i, lookup := range lookups {
		wg.Go(func() {
			found[i], errs[i] = lookup()
		})
	}
	wg.Wait()

	// Any object found answers the question, even when another lookup failed
	if slices.Contains(found, true) {
		return true, nil
	}
	err := errors.Join(errs...)
	if err != nil {
		return false, err
	}
	return false, nil
}

// OpenOriginal opens an image's source for reading, and the caller owns the returned reader and must close it
//
// The source is streamed rather than buffered because it is written straight to the local file its variants are encoded from, and nothing here bounds its size: how much of it is kept is the caller's decision
//
// When consistent is set the read bypasses whatever cache the store serves its reads from
// It is what a read performed right after the upload needs: a cached read can be answered from before the write, and a store that caches its answer for a minute would otherwise turn a source that does exist into a source this process never sees
func (s *Store) OpenOriginal(ctx context.Context, namespace string, imageID uuid.UUID, format string, consistent bool) (io.ReadCloser, error) {
	key := s.OriginalKey(namespace, imageID, format)

	var (
		body io.ReadCloser
		err  error
	)
	if consistent {
		body, _, err = s.originals.GetConsistent(ctx, key)
	} else {
		body, _, err = s.originals.Get(ctx, key)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the source image '%s': %w", key, err)
	}

	return body, nil
}

// RemoveOriginal deletes a source image that was saved by SaveOriginal
// It's used to clean up after a request that was rejected once its image had already been written
func (s *Store) RemoveOriginal(ctx context.Context, namespace string, imageID uuid.UUID, format string) error {
	key := s.OriginalKey(namespace, imageID, format)

	err := s.originals.Delete(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to remove the source image '%s': %w", key, err)
	}

	return nil
}

// SaveVariant writes one generated variant of an image
func (s *Store) SaveVariant(ctx context.Context, namespace string, imageID uuid.UUID, width int, height int, format string, data []byte, contentType string) error {
	key := s.AssetKey(namespace, imageID, width, height, format)

	_, err := s.variants.Put(ctx, key, bytes.NewReader(data), int64(len(data)), contentType)
	if err != nil {
		return fmt.Errorf("failed to write the variant '%s': %w", key, err)
	}

	return nil
}

// ReadVariant reads back one generated variant of an image
func (s *Store) ReadVariant(ctx context.Context, namespace string, imageID uuid.UUID, width int, height int, format string) ([]byte, error) {
	return s.read(ctx, s.AssetKey(namespace, imageID, width, height, format), false)
}

// VariantExists reports whether one generated variant is already stored
func (s *Store) VariantExists(ctx context.Context, namespace string, imageID uuid.UUID, width int, height int, format string) (bool, error) {
	key := s.AssetKey(namespace, imageID, width, height, format)
	exists, err := s.variants.Exists(ctx, key)
	if err != nil {
		return false, fmt.Errorf("failed to look up the variant '%s': %w", key, err)
	}
	return exists, nil
}

// RemoveVariant deletes one generated variant
// Compensation can call this repeatedly because every supported backend treats a missing object as a successful delete
func (s *Store) RemoveVariant(ctx context.Context, namespace string, imageID uuid.UUID, width int, height int, format string) error {
	key := s.AssetKey(namespace, imageID, width, height, format)

	err := s.variants.Delete(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to remove the variant '%s': %w", key, err)
	}

	return nil
}

// WriteManifest encodes and writes the manifest describing an image, next to its variants
func (s *Store) WriteManifest(ctx context.Context, namespace string, imageID uuid.UUID, manifest any) error {
	enc, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to encode manifest: %w", err)
	}

	return s.WriteManifestRaw(ctx, namespace, imageID, enc)
}

// WriteManifestRaw writes a manifest that is already encoded, which is how a compensation puts back the exact bytes a job found
func (s *Store) WriteManifestRaw(ctx context.Context, namespace string, imageID uuid.UUID, data []byte) error {
	key := s.ManifestKey(namespace, imageID)
	_, err := s.variants.Put(ctx, key, bytes.NewReader(data), int64(len(data)), manifestContentType)
	if err != nil {
		return fmt.Errorf("failed to write the manifest '%s': %w", key, err)
	}

	return nil
}

// ReadManifest reads back the manifest of an image
func (s *Store) ReadManifest(ctx context.Context, namespace string, imageID uuid.UUID) ([]byte, error) {
	return s.read(ctx, s.ManifestKey(namespace, imageID), false)
}

// ReadManifestConsistent bypasses cached reads, which a job needs when it builds on the state the previous one left behind
func (s *Store) ReadManifestConsistent(ctx context.Context, namespace string, imageID uuid.UUID) ([]byte, error) {
	return s.read(ctx, s.ManifestKey(namespace, imageID), true)
}

// RemoveManifest deletes the manifest stored for an image
// Compensation can call this repeatedly because every supported backend treats a missing object as a successful delete
func (s *Store) RemoveManifest(ctx context.Context, namespace string, imageID uuid.UUID) error {
	key := s.ManifestKey(namespace, imageID)

	err := s.variants.Delete(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to remove the manifest '%s': %w", key, err)
	}

	return nil
}

func (s *Store) read(ctx context.Context, key string, consistent bool) ([]byte, error) {
	var body io.ReadCloser
	var err error
	if consistent {
		body, _, err = s.variants.GetConsistent(ctx, key)
	} else {
		body, _, err = s.variants.Get(ctx, key)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read '%s': %w", key, err)
	}
	defer body.Close() //nolint:errcheck

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("failed to read '%s': %w", key, err)
	}

	return data, nil
}

// Report whether a source image is stored at a key already
//
// This is a metadata lookup rather than a read on purpose
// A read for an object that isn't there yet goes through whatever cache serves the reads of the store, and a store that caches its answer leaves a "not found" behind for the reads that come after the upload
func (s *Store) originalExists(ctx context.Context, key string) (bool, error) {
	exists, err := s.originals.Exists(ctx, key)
	if err != nil {
		return false, fmt.Errorf("failed to look up the source image '%s': %w", key, err)
	}

	return exists, nil
}

// Remove a source image without reporting the outcome, used on the paths that are already failing
func (s *Store) removeOriginalQuietly(ctx context.Context, key string) {
	// The context of a failed request is often already canceled, so the delete gets one of its own
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_ = s.originals.Delete(ctx, key)
}
