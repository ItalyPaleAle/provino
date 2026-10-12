// Package sourcecache keeps the source image of a workflow on the local disk for as long as its variants are being generated
//
// Every variant of an image is encoded from the same source, and without this each one would download that source again: a workflow producing a dozen variants would pull the same object out of the store a dozen times
// The workflow downloads it once instead, before it schedules anything, and every step that follows reads the file it left behind
//
// The cache is local to one host and holds nothing that cannot be downloaded again, so a miss, a sweep or a lost disk only ever costs one more download
package sourcecache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
	"uuid"

	"github.com/italypaleale/provino/internal/store"
)

const (
	// Permissions of the cache folder and of the files in it
	// Source images of every site pass through here, and nothing outside this process ever reads them
	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600

	// Suffix every cached file carries, so the folder says what it holds to whoever finds it on a host
	cacheFileSuffix = ".orig"
	// Names the temporary file a download is streamed into before it is renamed onto its final path
	tempFilePattern = "download-*"

	// Shortest interval the janitor sweeps at, so a very short maximum age doesn't turn it into a busy loop
	minSweepInterval = time.Minute
	// Longest interval the janitor sweeps at, so a cache left behind by a stopped host is still reclaimed within a sensible time
	maxSweepInterval = 15 * time.Minute
)

// Source is the part of the image store the cache downloads through
// It is an interface so the cache can be exercised without an object store behind it
type Source interface {
	// OpenOriginal opens an image's source for reading, optionally bypassing the cache the store serves its reads from
	OpenOriginal(ctx context.Context, namespace string, imageID uuid.UUID, format string, consistent bool) (io.ReadCloser, error)
}

// Cache is the folder the source images are downloaded into
type Cache struct {
	source        Source
	dir           string
	maxSize       int64
	maxAge        time.Duration
	sweepInterval time.Duration
	log           *slog.Logger
}

// Options contains the options for New
type Options struct {
	// Store the source images are downloaded from
	Source Source
	// Folder the source images are cached in, which is created if it doesn't exist
	Dir string
	// Maximum size of a source image, in bytes
	// An object larger than this is refused rather than cached, because it is larger than what this process is willing to encode anyway
	MaxSize int64
	// How long a cached file survives when nothing deletes it
	// It bounds what a host that stopped in the middle of a workflow leaves behind, and a later reader downloads a swept source again
	MaxAge time.Duration
	// Logger the cache reports to
	Logger *slog.Logger
}

// New creates a Cache and the folder it caches into
func New(opts Options) (*Cache, error) {
	if opts.Source == nil {
		return nil, errors.New("option Source is required")
	}
	if opts.Dir == "" {
		return nil, errors.New("option Dir is required")
	}
	if opts.MaxSize <= 0 {
		return nil, errors.New("option MaxSize must be greater than zero")
	}
	if opts.MaxAge <= 0 {
		return nil, errors.New("option MaxAge must be greater than zero")
	}

	// The folder is created at startup rather than at the first download, so a host whose cache folder isn't writable fails to start instead of failing every workflow
	err := os.MkdirAll(opts.Dir, dirPerm)
	if err != nil {
		return nil, fmt.Errorf("failed to create the source image cache folder '%s': %w", opts.Dir, err)
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Cache{
		source:        opts.Source,
		dir:           opts.Dir,
		maxSize:       opts.MaxSize,
		maxAge:        opts.MaxAge,
		sweepInterval: sweepInterval(opts.MaxAge),
		log:           logger.With(slog.String("scope", "sourcecache")),
	}, nil
}

// Dir returns the folder the source images are cached in, for logging
func (c *Cache) Dir() string {
	return c.dir
}

// Path returns the file one image's source is cached at
//
// The name is built from two identifiers that are already parsed UUIDs, so nothing that reaches here can name a file outside the cache folder
// The format the source was uploaded in is deliberately not part of it: an image has exactly one source, and the encoder recognizes it from its own content rather than from its name
func (c *Cache) Path(namespace string, imageID uuid.UUID) string {
	return filepath.Join(c.dir, namespace+"_"+imageID.String()+cacheFileSuffix)
}

// Prefetch downloads an image's source unless it is cached already
//
// This is the step that makes a workflow download its source once: everything it schedules afterwards reads the file this leaves behind
func (c *Cache) Prefetch(ctx context.Context, namespace string, imageID uuid.UUID, format string) error {
	_, err := os.Stat(c.Path(namespace, imageID))
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("failed to look up the cached source image: %w", err)
	}

	return c.download(ctx, namespace, imageID, format)
}

// Read returns an image's source, downloading it first when it isn't cached
//
// A variant runs on whichever host Francis placed its actor on, which isn't necessarily the one that prefetched the source, so a miss here is ordinary rather than exceptional
func (c *Cache) Read(ctx context.Context, namespace string, imageID uuid.UUID, format string) ([]byte, error) {
	data, err := c.readFile(namespace, imageID)
	switch {
	case err == nil:
		return data, nil
	case errors.Is(err, store.ErrTooLarge):
		// The limit is this process's own, so downloading the same object again can only reach the same verdict
		return nil, err
	case !errors.Is(err, fs.ErrNotExist):
		// A cached file that can't be read is not worth keeping, and downloading it again is what the miss below does anyway
		c.log.WarnContext(ctx, "Failed to read a cached source image, downloading it again",
			slog.String("imageId", imageID.String()),
			slog.Any("error", err),
		)
		c.removeQuietly(ctx, namespace, imageID)
	}

	err = c.download(ctx, namespace, imageID, format)
	if err != nil {
		return nil, err
	}

	return c.readFile(namespace, imageID)
}

// Remove deletes the cached source of one image, and does nothing when it isn't cached
//
// The workflow calls this when it terminates, which is the point at which the last variant that could have needed the source has been generated
// It only ever reaches this host's copy: a variant placed elsewhere downloaded one of its own, and that one is left to the janitor of the host that holds it
func (c *Cache) Remove(namespace string, imageID uuid.UUID) error {
	err := os.Remove(c.Path(namespace, imageID))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("failed to remove the cached source image: %w", err)
	}

	return nil
}

// CreateTemp creates a temporary file in the cache folder, which a caller fills and then publishes with Adopt
//
// An upload passes through this process on its way to the store, so writing a copy here as it streams by saves the job that follows from downloading the same bytes again
// The file is created in the cache folder itself so the rename in Adopt stays within one file system, and it carries the pattern of a download in progress so the janitor reclaims it if the caller abandons it
func (c *Cache) CreateTemp() (*os.File, error) {
	tmp, err := os.CreateTemp(c.dir, tempFilePattern)
	if err != nil {
		return nil, fmt.Errorf("failed to create a temporary file in '%s': %w", c.dir, err)
	}

	err = tmp.Chmod(filePerm)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("failed to set the permissions of '%s': %w", tmp.Name(), err)
	}

	return tmp, nil
}

// Adopt publishes a file created with CreateTemp as the cached source of an image
// The caller must have closed the file, and on success it no longer exists under its temporary name
func (c *Cache) Adopt(tmpName string, namespace string, imageID uuid.UUID) error {
	err := os.Rename(tmpName, c.Path(namespace, imageID))
	if err != nil {
		return fmt.Errorf("failed to publish the cached source image: %w", err)
	}

	return nil
}

// Run sweeps the cache folder on an interval until ctx is canceled
func (c *Cache) Run(ctx context.Context) error {
	c.log.DebugContext(ctx, "Source image cache started",
		slog.String("dir", c.dir),
		slog.Duration("maxAge", c.maxAge),
		slog.Duration("sweepInterval", c.sweepInterval),
	)

	ticker := time.NewTicker(c.sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			err := c.Sweep(ctx)
			if err != nil {
				c.log.WarnContext(ctx, "Failed to sweep the source image cache", slog.Any("error", err))
			}
		}
	}
}

// Sweep deletes the cached sources that have outlived the workflow they were downloaded for
//
// A workflow deletes its own source when it terminates, but it only does so on the host it terminated on: a variant placed on another host downloaded the source there, and nothing on that host is ever told the workflow is over
// A process killed between the download and the end of its workflow leaves a file behind for the same reason, so what decides here is the age of a file rather than the workflow it belongs to
func (c *Cache) Sweep(ctx context.Context) error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return fmt.Errorf("failed to list the source image cache folder '%s': %w", c.dir, err)
	}

	cutoff := time.Now().Add(-c.maxAge)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		// A file that disappears while the folder is being listed was removed by the workflow that owned it, which is what this is here to do anyway
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}

		err = os.Remove(filepath.Join(c.dir, entry.Name()))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.log.WarnContext(ctx, "Failed to remove a stale cached source image",
				slog.String("file", entry.Name()),
				slog.Any("error", err),
			)
			continue
		}

		removed++
	}

	if removed > 0 {
		c.log.InfoContext(ctx, "Removed stale cached source images", slog.Int("count", removed))
	}

	return nil
}

// Download an image's source into the cache folder
//
// A download that fails is tried once more against the store itself, bypassing the cache the first one went through
// The workflow starts right after the upload, so a cached read can still be answered from before it, and a store that caches its answer for a minute would otherwise turn a source that does exist into a source this host never sees
func (c *Cache) download(ctx context.Context, namespace string, imageID uuid.UUID, format string) error {
	err := c.downloadOnce(ctx, namespace, imageID, format, false)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrTooLarge):
		// The limit is this process's own, so downloading the same object again can only reach the same verdict
		return err
	}

	c.log.WarnContext(ctx, "Failed to download a source image, retrying the download against the store itself",
		slog.String("imageId", imageID.String()),
		slog.Any("error", err),
	)

	return c.downloadOnce(ctx, namespace, imageID, format, true)
}

// Perform one attempt at downloading an image's source
//
// The object is streamed into a temporary file in the cache folder and renamed onto its final path once it is complete, so a reader only ever opens a whole file and an interrupted download leaves nothing behind
// The rename is also what makes two downloads of the same image safe, which happens whenever variants of one workflow start together on a host that hasn't cached it yet: the second one replaces the first with an identical file, and a reader that already opened the first keeps reading it
func (c *Cache) downloadOnce(ctx context.Context, namespace string, imageID uuid.UUID, format string, consistent bool) error {
	body, err := c.source.OpenOriginal(ctx, namespace, imageID, format, consistent)
	if err != nil {
		return fmt.Errorf("failed to read the source image: %w", err)
	}
	defer body.Close() //nolint:errcheck

	// The temporary file is created in the cache folder itself, so the rename that publishes it stays within one file system, which is what makes that rename atomic
	tmp, err := os.CreateTemp(c.dir, tempFilePattern)
	if err != nil {
		return fmt.Errorf("failed to create a temporary file in '%s': %w", c.dir, err)
	}
	tmpName := tmp.Name()

	// Every path out of here either renamed the temporary file away or is abandoning it, and an abandoned one must not be left to accumulate
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	err = tmp.Chmod(filePerm)
	if err != nil {
		return fmt.Errorf("failed to set the permissions of '%s': %w", tmpName, err)
	}

	// Copying one byte past the limit is what tells an object that is exactly at the limit apart from one that is over it
	written, err := io.Copy(tmp, io.LimitReader(body, c.maxSize+1))
	if err != nil {
		return fmt.Errorf("failed to download the source image: %w", err)
	}
	if written > c.maxSize {
		return store.ErrTooLarge
	}

	// The content reaches the disk before the file is given its final name, so a host that dies mid-download is never followed by one that reads a truncated source and reports it as an image that cannot be decoded
	err = tmp.Sync()
	if err != nil {
		return fmt.Errorf("failed to flush '%s': %w", tmpName, err)
	}
	err = tmp.Close()
	if err != nil {
		return fmt.Errorf("failed to close '%s': %w", tmpName, err)
	}

	err = os.Rename(tmpName, c.Path(namespace, imageID))
	if err != nil {
		return fmt.Errorf("failed to publish the cached source image: %w", err)
	}

	c.log.DebugContext(ctx, "Downloaded a source image into the cache",
		slog.String("imageId", imageID.String()),
		slog.Int64("size", written),
	)

	return nil
}

// Read one cached file, refusing the ones larger than what this process is willing to encode
// The size is taken from the open file rather than from the path, so a file replaced by a concurrent download can't make this read past what it measured
func (c *Cache) readFile(namespace string, imageID uuid.UUID) ([]byte, error) {
	f, err := os.Open(c.Path(namespace, imageID))
	if err != nil {
		// The caller tells a miss apart from a real failure, so this one keeps fs.ErrNotExist reachable
		return nil, err //nolint:wrapcheck
	}
	defer f.Close() //nolint:errcheck

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to measure the cached source image: %w", err)
	}

	// Only a regular file has a size worth comparing against the limit, so anything else is reported as unreadable and downloaded again rather than measured
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("the cached source image is not a regular file: %s", info.Mode().String())
	}
	if info.Size() > c.maxSize {
		return nil, store.ErrTooLarge
	}

	data := make([]byte, info.Size())
	_, err = io.ReadFull(f, data)
	if err != nil {
		return nil, fmt.Errorf("failed to read the cached source image: %w", err)
	}

	return data, nil
}

// Remove a cached file without reporting the outcome, used on the paths that are already recovering from a failure
func (c *Cache) removeQuietly(ctx context.Context, namespace string, imageID uuid.UUID) {
	err := c.Remove(namespace, imageID)
	if err != nil {
		c.log.WarnContext(ctx, "Failed to remove a cached source image",
			slog.String("imageId", imageID.String()),
			slog.Any("error", err),
		)
	}
}

// Pick how often the janitor sweeps from how long a cached file is allowed to live
// Sweeping several times per maximum age is what keeps a file from outliving it by much, and the bounds keep a very short or very long age from turning that into a busy loop or into a sweep that never comes
func sweepInterval(maxAge time.Duration) time.Duration {
	return min(max(maxAge/4, minSweepInterval), maxSweepInterval)
}
