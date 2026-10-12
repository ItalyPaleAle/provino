package sourcecache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/internal/store"
)

var (
	testNamespace = "6ba7b810-9dad-41d1-80b4-00c04fd430c8"
	testImageID   = uuid.MustParse("01a061a9-7384-4e97-aadf-232d1e006b83")
)

// A source whose object is served from memory, counting how each read reached it
// The consistent read can be made the only one that succeeds, which is what a store answering an upload from a stale cache looks like
type fakeSource struct {
	data       string
	failCached bool
	notFound   bool
	readErr    error

	cachedReads     atomic.Int32
	consistentReads atomic.Int32
}

func (f *fakeSource) OpenOriginal(_ context.Context, _ string, _ uuid.UUID, _ string, consistent bool) (io.ReadCloser, error) {
	if consistent {
		f.consistentReads.Add(1)
	} else {
		f.cachedReads.Add(1)
		if f.failCached {
			return nil, store.ErrNotFound
		}
	}

	if f.notFound {
		return nil, store.ErrNotFound
	}

	// A body that fails partway through is what a connection dropped mid-download gives the cache, so opening it succeeds and reading it doesn't
	if f.readErr != nil {
		return &failingReader{err: f.readErr}, nil //nolint:nilerr // The failure belongs to the body, not to opening it
	}

	return io.NopCloser(strings.NewReader(f.data)), nil
}

type failingReader struct {
	err error
}

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }
func (f *failingReader) Close() error             { return nil }

func newTestCache(t *testing.T, source Source, maxSize int64) *Cache {
	t.Helper()

	c, err := New(Options{
		Source:  source,
		Dir:     filepath.Join(t.TempDir(), "sources"),
		MaxSize: maxSize,
		MaxAge:  time.Hour,
		Logger:  testLogger(t),
	})
	require.NoError(t, err)

	return c
}

func TestNew(t *testing.T) {
	// The folder is created at startup so a host whose cache isn't writable fails there rather than on its first workflow
	t.Run("creates the cache folder", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "nested", "sources")
		_, err := New(Options{Source: &fakeSource{}, Dir: dir, MaxSize: 1024, MaxAge: time.Hour, Logger: testLogger(t)})
		require.NoError(t, err)
		assert.DirExists(t, dir)
	})

	t.Run("requires every option", func(t *testing.T) {
		valid := Options{Source: &fakeSource{}, Dir: t.TempDir(), MaxSize: 1024, MaxAge: time.Hour}

		invalid := map[string]func(o *Options){
			"no source":    func(o *Options) { o.Source = nil },
			"no folder":    func(o *Options) { o.Dir = "" },
			"no max size":  func(o *Options) { o.MaxSize = 0 },
			"no max age":   func(o *Options) { o.MaxAge = 0 },
			"negative age": func(o *Options) { o.MaxAge = -time.Second },
		}
		for name, breakIt := range invalid {
			t.Run(name, func(t *testing.T) {
				opts := valid
				breakIt(&opts)
				_, err := New(opts)
				require.Error(t, err)
			})
		}
	})
}

// The file is named after two parsed UUIDs, so nothing that reaches the cache can name a file outside its folder
func TestPath(t *testing.T) {
	c := newTestCache(t, &fakeSource{}, 1024)

	path := c.Path(testNamespace, testImageID)

	assert.Equal(t, c.Dir(), filepath.Dir(path))
	assert.Equal(t, testNamespace+"_"+testImageID.String()+".orig", filepath.Base(path))
}

func TestPrefetch(t *testing.T) {
	t.Run("downloads the source once", func(t *testing.T) {
		source := &fakeSource{data: "image-data"}
		c := newTestCache(t, source, 1024)

		err := c.Prefetch(t.Context(), testNamespace, testImageID, ".jpg")
		require.NoError(t, err)
		assert.FileExists(t, c.Path(testNamespace, testImageID))

		// A workflow whose start job is delivered twice must not download the source again
		err = c.Prefetch(t.Context(), testNamespace, testImageID, ".jpg")
		require.NoError(t, err)
		assert.Equal(t, int32(1), source.cachedReads.Load())

		content, err := os.ReadFile(c.Path(testNamespace, testImageID)) //nolint:gosec // The path is inside a temporary folder this test created
		require.NoError(t, err)
		assert.Equal(t, "image-data", string(content))
	})

	// Nothing outside this process reads a source image, so the folder and its files stay private to the user the service runs as
	t.Run("keeps the cached file private", func(t *testing.T) {
		c := newTestCache(t, &fakeSource{data: "image-data"}, 1024)
		err := c.Prefetch(t.Context(), testNamespace, testImageID, ".jpg")
		require.NoError(t, err)

		info, err := os.Stat(c.Path(testNamespace, testImageID))
		require.NoError(t, err)
		assert.Equal(t, filePerm, info.Mode().Perm())
	})

	t.Run("reports a source that isn't there", func(t *testing.T) {
		c := newTestCache(t, &fakeSource{notFound: true}, 1024)

		err := c.Prefetch(t.Context(), testNamespace, testImageID, ".jpg")
		require.ErrorIs(t, err, store.ErrNotFound)
		assert.NoFileExists(t, c.Path(testNamespace, testImageID))
	})
}

func TestRead(t *testing.T) {
	t.Run("downloads a source that isn't cached", func(t *testing.T) {
		source := &fakeSource{data: "image-data"}
		c := newTestCache(t, source, 1024)

		data, err := c.Read(t.Context(), testNamespace, testImageID, ".jpg")
		require.NoError(t, err)
		assert.Equal(t, "image-data", string(data))
		assert.Equal(t, int32(1), source.cachedReads.Load())
	})

	// This is the point of the cache: every variant after the first reads the file the workflow left behind
	t.Run("reads a cached source without downloading it again", func(t *testing.T) {
		source := &fakeSource{data: "image-data"}
		c := newTestCache(t, source, 1024)

		err := c.Prefetch(t.Context(), testNamespace, testImageID, ".jpg")
		require.NoError(t, err)
		for range 5 {
			data, err := c.Read(t.Context(), testNamespace, testImageID, ".jpg")
			require.NoError(t, err)
			assert.Equal(t, "image-data", string(data))
		}

		assert.Equal(t, int32(1), source.cachedReads.Load())
		assert.Zero(t, source.consistentReads.Load())
	})

	// A source larger than what this process encodes is refused rather than cached, and reading it again cannot reach another verdict
	t.Run("refuses a source over the maximum size", func(t *testing.T) {
		source := &fakeSource{data: "image-data"}
		c := newTestCache(t, source, 4)

		_, err := c.Read(t.Context(), testNamespace, testImageID, ".jpg")
		require.ErrorIs(t, err, store.ErrTooLarge)
		assert.NoFileExists(t, c.Path(testNamespace, testImageID))
		assert.Equal(t, int32(1), source.cachedReads.Load())
		assert.Zero(t, source.consistentReads.Load())
	})

	// A source exactly at the limit is the one an off-by-one gets wrong
	t.Run("accepts a source exactly at the maximum size", func(t *testing.T) {
		c := newTestCache(t, &fakeSource{data: "image-data"}, int64(len("image-data")))

		data, err := c.Read(t.Context(), testNamespace, testImageID, ".jpg")
		require.NoError(t, err)
		assert.Equal(t, "image-data", string(data))
	})

	t.Run("replaces a cached file that cannot be read", func(t *testing.T) {
		c := newTestCache(t, &fakeSource{data: "image-data"}, 1024)

		// A folder where the file should be is unreadable in a way that isn't a miss, which is what a corrupted cache looks like from here
		err := os.Mkdir(c.Path(testNamespace, testImageID), dirPerm)
		require.NoError(t, err)

		data, err := c.Read(t.Context(), testNamespace, testImageID, ".jpg")
		require.NoError(t, err)
		assert.Equal(t, "image-data", string(data))
	})
}

// A store fronted by a cache can answer a read from before the upload the workflow was started by
func TestDownloadRetriesAgainstTheStore(t *testing.T) {
	t.Run("recovers a cached read that missed the object", func(t *testing.T) {
		source := &fakeSource{data: "image-data", failCached: true}
		c := newTestCache(t, source, 1024)

		data, err := c.Read(t.Context(), testNamespace, testImageID, ".jpg")
		require.NoError(t, err)
		assert.Equal(t, "image-data", string(data))

		// The cached read is what every attempt starts with, since it's the cheaper one and the one that almost always succeeds
		assert.Equal(t, int32(1), source.cachedReads.Load())
		assert.Equal(t, int32(1), source.consistentReads.Load())
	})

	t.Run("recovers a download that failed halfway through", func(t *testing.T) {
		source := &fakeSource{data: "image-data", readErr: errors.New("connection reset")}
		c := newTestCache(t, source, 1024)

		_, err := c.Read(t.Context(), testNamespace, testImageID, ".jpg")
		require.Error(t, err)

		// Both attempts failed, and neither left a partial file behind for a variant to encode from
		assert.Equal(t, int32(1), source.cachedReads.Load())
		assert.Equal(t, int32(1), source.consistentReads.Load())
		assert.NoFileExists(t, c.Path(testNamespace, testImageID))
		assertNoTempFiles(t, c)
	})

	t.Run("reports an object that neither read found", func(t *testing.T) {
		source := &fakeSource{notFound: true}
		c := newTestCache(t, source, 1024)

		err := c.Prefetch(t.Context(), testNamespace, testImageID, ".jpg")
		require.ErrorIs(t, err, store.ErrNotFound)
		assert.Equal(t, int32(1), source.cachedReads.Load())
		assert.Equal(t, int32(1), source.consistentReads.Load())
	})
}

// Variants of one workflow start together, so a host that hasn't cached the source downloads it several times at once
// The rename is what makes that safe: every reader sees a whole file, and the one it sees is always complete
func TestConcurrentDownloadsLeaveOneCompleteFile(t *testing.T) {
	source := &fakeSource{data: strings.Repeat("image-data", 4096)}
	c := newTestCache(t, source, 1<<20)

	var wg sync.WaitGroup
	results := make([][]byte, 8)
	for i := range results {
		wg.Go(func() {
			data, err := c.Read(t.Context(), testNamespace, testImageID, ".jpg")
			assert.NoError(t, err)
			results[i] = data
		})
	}
	wg.Wait()

	for _, data := range results {
		assert.Equal(t, source.data, string(data))
	}
	assertNoTempFiles(t, c)
}

func TestRemove(t *testing.T) {
	c := newTestCache(t, &fakeSource{data: "image-data"}, 1024)
	err := c.Prefetch(t.Context(), testNamespace, testImageID, ".jpg")
	require.NoError(t, err)

	t.Run("removes the cached source", func(t *testing.T) {
		err := c.Remove(testNamespace, testImageID)
		require.NoError(t, err)
		assert.NoFileExists(t, c.Path(testNamespace, testImageID))
	})

	// The workflow that terminates twice removes the same file twice, and a variant placed elsewhere never cached it here at all
	t.Run("ignores a source that is already gone", func(t *testing.T) {
		err := c.Remove(testNamespace, testImageID)
		require.NoError(t, err)
	})
}

func TestSweep(t *testing.T) {
	c := newTestCache(t, &fakeSource{data: "image-data"}, 1024)
	err := c.Prefetch(t.Context(), testNamespace, testImageID, ".jpg")
	require.NoError(t, err)

	fresh := c.Path(testNamespace, testImageID)
	stale := c.Path(testNamespace, uuid.NewV4())
	err = os.WriteFile(stale, []byte("left behind"), filePerm)
	require.NoError(t, err)
	old := time.Now().Add(-2 * time.Hour)
	err = os.Chtimes(stale, old, old)
	require.NoError(t, err)

	err = c.Sweep(t.Context())
	require.NoError(t, err)

	// The workflow that downloaded the stale file terminated on another host, or never terminated at all, so nothing else was ever going to remove it
	assert.NoFileExists(t, stale)
	assert.FileExists(t, fresh)
}

// The janitor stops with the host that started it, and nothing it does outlives that
func TestRunStopsWithItsContext(t *testing.T) {
	c := newTestCache(t, &fakeSource{data: "image-data"}, 1024)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx)
	}()

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the janitor did not stop when its context was canceled")
	}
}

func TestSweepInterval(t *testing.T) {
	// A short maximum age must not turn the janitor into a busy loop, and a long one must not push a sweep out of reach
	assert.Equal(t, minSweepInterval, sweepInterval(time.Second))
	assert.Equal(t, maxSweepInterval, sweepInterval(24*time.Hour))
	assert.Equal(t, 5*time.Minute, sweepInterval(20*time.Minute))
}

// Every download either publishes its temporary file or removes it, so none of them is ever left in the folder
func assertNoTempFiles(t *testing.T, c *Cache) {
	t.Helper()

	prefix := strings.TrimSuffix(tempFilePattern, "*")
	entries, err := os.ReadDir(c.Dir())
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, strings.HasPrefix(entry.Name(), prefix), "a download left the temporary file %s behind", entry.Name())
	}
}

// Make sure the cache's own error paths stay reachable in a test, without their logs drowning the output
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.DiscardHandler)
}

// An upload writes its copy of the source through CreateTemp and Adopt, and a reader then finds it without downloading
func TestCacheAdoptPublishesAnUploadedSource(t *testing.T) {
	source := &fakeSource{}
	cache := newTestCache(t, source, 1<<20)
	imageID := uuid.NewV4()

	tmp, err := cache.CreateTemp()
	require.NoError(t, err)
	_, err = tmp.WriteString("uploaded bytes")
	require.NoError(t, err)
	err = tmp.Close()
	require.NoError(t, err)

	err = cache.Adopt(tmp.Name(), testNamespace, imageID)
	require.NoError(t, err)

	data, err := cache.Read(t.Context(), testNamespace, imageID, ".png")
	require.NoError(t, err)
	assert.Equal(t, "uploaded bytes", string(data))
	assert.Zero(t, source.cachedReads.Load()+source.consistentReads.Load(), "an adopted source must not be downloaded again")

	_, err = os.Stat(tmp.Name())
	require.ErrorIs(t, err, os.ErrNotExist)
}
