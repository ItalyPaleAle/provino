package store

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Report the permissions of a file or folder
func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Mode().Perm()
}

func TestLocalStoragePermissions(t *testing.T) {
	t.Run("the defaults keep objects to the service's user", func(t *testing.T) {
		dir := t.TempDir()
		s, err := New(NewOpts{OriginalsDSN: filepath.Join(dir, "originals"), VariantsDSN: "fs:" + filepath.Join(dir, "variants")})
		require.NoError(t, err)

		err = s.SaveVariant(t.Context(), testNamespace, testImageID, 100, 75, "webp", []byte("data"), "image/webp")
		require.NoError(t, err)

		key := s.AssetKey(testNamespace, testImageID, 100, 75, "webp")
		assert.Equal(t, fs.FileMode(0o600), modeOf(t, storedPath(dir, "variants", key)))
		assert.Equal(t, fs.FileMode(0o750), modeOf(t, filepath.Dir(storedPath(dir, "variants", key))))
		assert.NotContains(t, s.VariantsDescription(), "permissions")
	})

	t.Run("the DSN sets the permissions of files and folders", func(t *testing.T) {
		dir := t.TempDir()
		s, err := New(NewOpts{
			OriginalsDSN: filepath.Join(dir, "originals"),
			VariantsDSN:  "fs:" + filepath.Join(dir, "variants") + "?fileMode=0644&dirMode=0755",
		})
		require.NoError(t, err)
		assert.Contains(t, s.VariantsDescription(), "permissions 0644 for files and 0755 for folders")

		err = s.SaveVariant(t.Context(), testNamespace, testImageID, 100, 75, "webp", []byte("data"), "image/webp")
		require.NoError(t, err)
		err = s.WriteManifest(t.Context(), testNamespace, testImageID, map[string]string{"version": "provino/v1"})
		require.NoError(t, err)

		// Every object, and every folder from the root down to it, gets the configured permissions
		for _, key := range []string{s.AssetKey(testNamespace, testImageID, 100, 75, "webp"), s.ManifestKey(testNamespace, testImageID)} {
			path := storedPath(dir, "variants", key)
			assert.Equal(t, fs.FileMode(0o644), modeOf(t, path), key)
			for folder := filepath.Dir(path); folder != filepath.Join(dir, "variants"); folder = filepath.Dir(folder) {
				assert.Equal(t, fs.FileMode(0o755), modeOf(t, folder), folder)
			}
		}
		assert.Equal(t, fs.FileMode(0o755), modeOf(t, filepath.Join(dir, "variants")))

		// The other store keeps the defaults, since its DSN asks for nothing
		_, _, err = s.SaveOriginal(t.Context(), testNamespace, testImageID, "jpg", strings.NewReader("image"), 1024, "image/jpeg")
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o600), modeOf(t, storedPath(dir, "originals", s.OriginalKey(testNamespace, testImageID, "jpg"))))
	})

	t.Run("a bare path takes the options too", func(t *testing.T) {
		dir := t.TempDir()
		s, err := New(NewOpts{OriginalsDSN: filepath.Join(dir, "originals") + "?fileMode=640", VariantsDSN: filepath.Join(dir, "variants")})
		require.NoError(t, err)

		key, _, err := s.SaveOriginal(t.Context(), testNamespace, testImageID, "jpg", strings.NewReader("image"), 1024, "image/jpeg")
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o640), modeOf(t, storedPath(dir, "originals", key)))
		// The folder isn't named after the options
		assert.NoDirExists(t, filepath.Join(dir, "originals?fileMode=640"))
	})

	invalid := map[string]string{
		"an unknown option":            "?mode=0644",
		"a mode that isn't octal":      "?fileMode=0899",
		"a mode past 0777":             "?dirMode=01777",
		"an empty mode":                "?fileMode=",
		"files the service can't read": "?fileMode=0244",
		"folders it can't enter":       "?dirMode=0655",
	}
	for name, options := range invalid {
		t.Run("refuses "+name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := New(NewOpts{OriginalsDSN: filepath.Join(dir, "originals"), VariantsDSN: filepath.Join(dir, "variants") + options})
			require.Error(t, err)

			// Nothing is created for a DSN that's refused
			assert.NoDirExists(t, filepath.Join(dir, "variants"))
		})
	}
}
