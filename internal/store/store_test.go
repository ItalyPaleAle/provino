package store

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/variant"
)

var (
	testNamespace = "6ba7b810-9dad-41d1-80b4-00c04fd430c8"
	testImageID   = uuid.MustParse("01a061a9-7384-4e97-aadf-232d1e006b83")
	testFileID    = "3f9a0c1e"
)

// The folder levels a local backend adds, which are the leading characters of the image ID
const testShard = "01/a0/"

// Create a store whose two backends are folders in a temporary directory
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()

	dir := t.TempDir()
	s, err := New(NewOpts{
		OriginalsDSN: filepath.Join(dir, "originals"),
		VariantsDSN:  filepath.Join(dir, "variants"),
	})
	require.NoError(t, err)

	return s, dir
}

// Path an object with a given key has on disk, for the assertions that read the files directly
func storedPath(dir string, backend string, key string) string {
	return filepath.Join(dir, backend, filepath.FromSlash(key))
}

func TestNew(t *testing.T) {
	t.Run("creates the folders", func(t *testing.T) {
		_, dir := newTestStore(t)
		assert.DirExists(t, filepath.Join(dir, "originals"))
		assert.DirExists(t, filepath.Join(dir, "variants"))
	})

	t.Run("requires both DSNs", func(t *testing.T) {
		_, err := New(NewOpts{OriginalsDSN: "", VariantsDSN: t.TempDir()})
		require.Error(t, err)

		_, err = New(NewOpts{OriginalsDSN: t.TempDir(), VariantsDSN: ""})
		require.Error(t, err)
	})
}

// The keys are the shared layout's, since applications read these same objects out of the same store
func TestKeys(t *testing.T) {
	s, _ := newTestStore(t)
	prefix := testNamespace + "/" + testShard

	assert.Equal(t, prefix+testImageID.String()+"_orig.jpg", s.OriginalKey(testNamespace, testImageID, ".JPG"))
	assert.Equal(t, prefix+testImageID.String()+"_orig.jpg", s.OriginalKey(testNamespace, testImageID, "jpg"))
	assert.Equal(t, prefix+testImageID.String()+"_200x150_3f9a0c1e.webp", s.AssetKey(testNamespace, testImageID, 200, 150, testFileID, variant.FormatWebP))
	assert.Equal(t, prefix+testImageID.String()+"_manifest.json", s.ManifestKey(testNamespace, testImageID))

	// The layout does not vary by backend, so a remote store addresses the very same key as the local one
	remote, err := New(NewOpts{
		OriginalsDSN: "s3://key:secret@s3.us-east-1.amazonaws.com/originals",
		VariantsDSN:  "s3://key:secret@s3.us-east-1.amazonaws.com/variants",
	})
	require.NoError(t, err)
	assert.Equal(t, s.OriginalKey(testNamespace, testImageID, ".jpg"), remote.OriginalKey(testNamespace, testImageID, ".jpg"))
	assert.Equal(t, s.AssetKey(testNamespace, testImageID, 200, 150, testFileID, variant.FormatWebP), remote.AssetKey(testNamespace, testImageID, 200, 150, testFileID, variant.FormatWebP))
}

func TestSaveOriginal(t *testing.T) {
	t.Run("saves the image", func(t *testing.T) {
		s, dir := newTestStore(t)

		key, size, err := s.SaveOriginal(t.Context(), testNamespace, testImageID, ".JPG", strings.NewReader("image-data"), 1024, "image/jpeg")
		require.NoError(t, err)
		assert.Equal(t, s.OriginalKey(testNamespace, testImageID, ".jpg"), key)
		assert.Equal(t, int64(10), size)

		content, err := os.ReadFile(storedPath(dir, "originals", key)) //nolint:gosec // The path is inside a temporary folder this test created
		require.NoError(t, err)
		assert.Equal(t, "image-data", string(content))
	})

	t.Run("rejects an image that is too large", func(t *testing.T) {
		s, dir := newTestStore(t)

		_, _, err := s.SaveOriginal(t.Context(), testNamespace, testImageID, ".jpg", strings.NewReader("image-data"), 4, "image/jpeg")
		require.ErrorIs(t, err, ErrTooLarge)

		// The partial object is removed, so a rejected request leaves nothing behind and the image ID stays usable
		assert.NoFileExists(t, storedPath(dir, "originals", s.OriginalKey(testNamespace, testImageID, ".jpg")))
	})

	t.Run("rejects an empty image", func(t *testing.T) {
		s, dir := newTestStore(t)

		_, _, err := s.SaveOriginal(t.Context(), testNamespace, testImageID, ".jpg", strings.NewReader(""), 1024, "image/jpeg")
		require.Error(t, err)
		assert.NoFileExists(t, storedPath(dir, "originals", s.OriginalKey(testNamespace, testImageID, ".jpg")))
	})
}

func TestImageExists(t *testing.T) {
	t.Run("nothing stored", func(t *testing.T) {
		s, _ := newTestStore(t)

		exists, err := s.ImageExists(t.Context(), testNamespace, testImageID)
		require.NoError(t, err)
		assert.False(t, exists)
	})

	// The key of an original carries its format, so an original in any format counts, not only the one a new upload would have
	for _, format := range []string{"jpg", "png", "webp", "avif", "heic", "heif"} {
		t.Run("an original in "+format, func(t *testing.T) {
			s, _ := newTestStore(t)
			_, _, err := s.SaveOriginal(t.Context(), testNamespace, testImageID, format, strings.NewReader("image-data"), 1024, "application/octet-stream")
			require.NoError(t, err)

			exists, err := s.ImageExists(t.Context(), testNamespace, testImageID)
			require.NoError(t, err)
			assert.True(t, exists)
		})
	}

	t.Run("a manifest without an original", func(t *testing.T) {
		s, _ := newTestStore(t)
		err := s.WriteManifest(t.Context(), testNamespace, testImageID, map[string]string{"version": "provino/v1"})
		require.NoError(t, err)

		exists, err := s.ImageExists(t.Context(), testNamespace, testImageID)
		require.NoError(t, err)
		assert.True(t, exists)
	})

	t.Run("another image in the same namespace", func(t *testing.T) {
		s, _ := newTestStore(t)
		_, _, err := s.SaveOriginal(t.Context(), testNamespace, uuid.NewV4(), "jpg", strings.NewReader("image-data"), 1024, "image/jpeg")
		require.NoError(t, err)

		exists, err := s.ImageExists(t.Context(), testNamespace, testImageID)
		require.NoError(t, err)
		assert.False(t, exists)
	})
}

func TestOpenOriginal(t *testing.T) {
	s, _ := newTestStore(t)

	_, _, err := s.SaveOriginal(t.Context(), testNamespace, testImageID, ".jpg", strings.NewReader("image-data"), 1024, "image/jpeg")
	require.NoError(t, err)

	read := func(t *testing.T, namespace string, imageID uuid.UUID, consistent bool) (string, error) {
		t.Helper()

		body, err := s.OpenOriginal(t.Context(), namespace, imageID, ".jpg", consistent)
		if err != nil {
			return "", err
		}
		defer body.Close() //nolint:errcheck

		data, err := io.ReadAll(body)
		require.NoError(t, err)

		return string(data), nil
	}

	// A local backend has no cache in front of it, so both reads reach the same object
	// What each of them does with a store that has one is the object store's own concern, and the retry that relies on the difference belongs to the source cache
	t.Run("reads the image back", func(t *testing.T) {
		for _, consistent := range []bool{false, true} {
			data, err := read(t, testNamespace, testImageID, consistent)
			require.NoError(t, err)
			assert.Equal(t, "image-data", data)
		}
	})

	t.Run("reports a missing image", func(t *testing.T) {
		_, err := read(t, testNamespace, uuid.NewV4(), false)
		require.ErrorIs(t, err, ErrNotFound)
	})

	// The namespace is part of the key, so one namespace cannot reach another namespace's object
	t.Run("does not reach another namespace's image", func(t *testing.T) {
		_, err := read(t, "other-namespace", testImageID, false)
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestRemoveOriginal(t *testing.T) {
	s, dir := newTestStore(t)

	key, _, sErr := s.SaveOriginal(t.Context(), testNamespace, testImageID, ".jpg", strings.NewReader("image-data"), 1024, "image/jpeg")
	require.NoError(t, sErr)

	t.Run("removes the image", func(t *testing.T) {
		err := s.RemoveOriginal(t.Context(), testNamespace, testImageID, ".jpg")
		require.NoError(t, err)
		assert.NoFileExists(t, storedPath(dir, "originals", key))
	})

	// Deletes are idempotent because the request that failed may have been retried
	t.Run("ignores an image that is already gone", func(t *testing.T) {
		err := s.RemoveOriginal(t.Context(), testNamespace, testImageID, ".jpg")
		require.NoError(t, err)
	})
}

func TestSaveVariant(t *testing.T) {
	t.Run("writes the variant", func(t *testing.T) {
		s, dir := newTestStore(t)
		key := s.AssetKey(testNamespace, testImageID, 200, 150, testFileID, variant.FormatWebP)

		err := s.SaveVariant(t.Context(), key, []byte("variant"), "image/webp")
		require.NoError(t, err)

		content, err := os.ReadFile(storedPath(dir, "variants", key)) //nolint:gosec // The path is inside a temporary folder this test created
		require.NoError(t, err)
		assert.Equal(t, "variant", string(content))

		exists, err := s.VariantExists(t.Context(), key)
		require.NoError(t, err)
		assert.True(t, exists)
		exists, err = s.VariantExists(t.Context(), s.AssetKey(testNamespace, testImageID, 200, 150, "0000beef", variant.FormatWebP))
		require.NoError(t, err)
		assert.False(t, exists)
	})

	// A retried job regenerates the same variant, so writing it twice must be allowed
	t.Run("overwrites a variant that exists already", func(t *testing.T) {
		s, _ := newTestStore(t)
		key := s.AssetKey(testNamespace, testImageID, 200, 150, testFileID, variant.FormatWebP)

		err := s.SaveVariant(t.Context(), key, []byte("first"), "image/webp")
		require.NoError(t, err)
		err = s.SaveVariant(t.Context(), key, []byte("second"), "image/webp")
		require.NoError(t, err)

		data, err := s.ReadVariant(t.Context(), key)
		require.NoError(t, err)
		assert.Equal(t, "second", string(data))
	})

	t.Run("removes a variant idempotently", func(t *testing.T) {
		s, _ := newTestStore(t)
		key := s.AssetKey(testNamespace, testImageID, 200, 150, testFileID, variant.FormatWebP)

		err := s.SaveVariant(t.Context(), key, []byte("variant"), "image/webp")
		require.NoError(t, err)
		err = s.RemoveVariant(t.Context(), key)
		require.NoError(t, err)
		err = s.RemoveVariant(t.Context(), key)
		require.NoError(t, err)

		_, err = s.ReadVariant(t.Context(), key)
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestManifest(t *testing.T) {
	s, dir := newTestStore(t)

	err := s.WriteManifest(t.Context(), testNamespace, testImageID, map[string]any{"status": "completed"})
	require.NoError(t, err)

	content, err := os.ReadFile(storedPath(dir, "variants", s.ManifestKey(testNamespace, testImageID))) //nolint:gosec // The path is inside a temporary folder this test created
	require.NoError(t, err)

	var manifest map[string]any
	err = json.Unmarshal(content, &manifest)
	require.NoError(t, err)
	assert.Equal(t, "completed", manifest["status"])

	// It reads back through the store as well, which is what the webhook step sends
	read, err := s.ReadManifest(t.Context(), testNamespace, testImageID)
	require.NoError(t, err)
	assert.JSONEq(t, string(content), string(read))

	// Compensation retries must be safe after the first delete has removed the object
	err = s.RemoveManifest(t.Context(), testNamespace, testImageID)
	require.NoError(t, err)
	err = s.RemoveManifest(t.Context(), testNamespace, testImageID)
	require.NoError(t, err)
	_, err = s.ReadManifest(t.Context(), testNamespace, testImageID)
	require.ErrorIs(t, err, ErrNotFound)
}
