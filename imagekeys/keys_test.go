package imagekeys

import (
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/variant"
)

var (
	siteID    = uuid.MustParse("6ba7b810-9dad-41d1-80b4-00c04fd430c8")
	namespace = siteID.String()
	imageID   = uuid.MustParse("3480e342-5b8a-4ec8-9b0d-a0b26a751691")

	fileID = "3f9a0c1e"
)

func TestLayout(t *testing.T) {
	// The two folder levels come from the first four characters of the image ID
	assert.Equal(t, namespace+"/34/80/", Prefix(namespace, imageID))
	assert.Equal(t, namespace+"/34/80/"+imageID.String()+"_orig.png", OriginalKey(namespace, imageID, "PNG"))
	assert.Equal(t, namespace+"/34/80/"+imageID.String()+"_1200x400_3f9a0c1e.webp", AssetKey(namespace, imageID, 1200, 400, fileID, variant.FormatWebP))
	assert.Equal(t, namespace+"/34/80/"+imageID.String()+"_manifest.json", ManifestKey(namespace, imageID))

	// Every object of one image sits under the prefix, so listing or deleting it reaches all of them
	for _, key := range []string{
		OriginalKey(namespace, imageID, "png"),
		AssetKey(namespace, imageID, 1200, 400, fileID, variant.FormatWebP),
		ManifestKey(namespace, imageID),
	} {
		assert.True(t, strings.HasPrefix(key, Prefix(namespace, imageID)), key)
	}
}

// The layout is one contract shared by both services and every backend, so nothing may reintroduce a per-store shape
func TestLayoutDoesNotVaryByBackend(t *testing.T) {
	site := uuid.MustParse("4c16dfe3-cefe-4057-8b29-55f08ecfe204")
	image := uuid.MustParse("50cefe14-49d3-4b54-bca6-3dfde5f3c1b8")

	assert.Equal(t,
		"4c16dfe3-cefe-4057-8b29-55f08ecfe204/50/ce/50cefe14-49d3-4b54-bca6-3dfde5f3c1b8_1536x2048_3f9a0c1e.avif",
		AssetKey(site.String(), image, 1536, 2048, fileID, variant.FormatAVIF),
	)
	assert.Equal(t,
		"4c16dfe3-cefe-4057-8b29-55f08ecfe204/50/ce/50cefe14-49d3-4b54-bca6-3dfde5f3c1b8_orig.jpg",
		OriginalKey(site.String(), image, "jpg"),
	)
}

// The folder levels are the leading characters of the image ID, which only spread evenly because those characters are random
// A time-ordered identifier would put everything created in the same period in one folder, which is the thing sharding exists to avoid
func TestShardFansOut(t *testing.T) {
	const n = 2000

	buckets := make(map[string]int, n)
	for range n {
		buckets[Prefix(namespace, uuid.NewV4())]++
	}

	// With 256 possible folders, 2000 identifiers land nearly evenly rather than piling into one
	assert.Greater(t, len(buckets), 200)

	largest := 0
	for _, count := range buckets {
		if count > largest {
			largest = count
		}
	}
	assert.Less(t, largest, n/50)
}

// A variant's key names the size the file came out at and its format, and ends with the file ID
func TestAssetKey(t *testing.T) {
	key := func(width int, height int, format variant.Format) string {
		name, found := strings.CutPrefix(AssetKey(namespace, imageID, width, height, fileID, format), Prefix(namespace, imageID)+imageID.String()+"_")
		require.True(t, found)
		return name
	}

	assert.Equal(t, "1200x400_3f9a0c1e.webp", key(1200, 400, variant.FormatWebP))
	assert.Equal(t, "800x800_3f9a0c1e.avif", key(800, 800, variant.FormatAVIF))
	assert.Equal(t, "600x244_3f9a0c1e.jpg", key(600, 244, variant.FormatJPEG))
	assert.Equal(t, "600x244_3f9a0c1e.png", key(600, 244, variant.FormatPNG))
}

func TestFileID(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for range 100 {
		id := NewFileID()
		err := ValidateFileID(id)
		require.NoError(t, err)
		seen[id] = struct{}{}
	}
	assert.Len(t, seen, 100)

	for _, id := range []string{"", "3f9a0c1", "3f9a0c1e0", "3F9A0C1E", "3f9a0c1g", "../../x", "3f9a/c1e"} {
		err := ValidateFileID(id)
		require.Error(t, err, id)
	}

	// The file ID is read back from the key, and a key an older layout named has none
	assert.Equal(t, fileID, FileIDOf(AssetKey(namespace, imageID, 1200, 400, fileID, variant.FormatWebP)))
	assert.Empty(t, FileIDOf(Prefix(namespace, imageID)+imageID.String()+"_600x400.webp"))
	assert.Empty(t, FileIDOf(ManifestKey(namespace, imageID)))
	assert.Empty(t, FileIDOf(OriginalKey(namespace, imageID, "png")))
	assert.Empty(t, FileIDOf(""))
}

func TestOwnsKey(t *testing.T) {
	other := uuid.NewV4()

	// Every key the layout builds for the image belongs to it, as do the keys of an older layout that are stored under the same prefix
	for _, key := range []string{
		OriginalKey(namespace, imageID, "png"),
		AssetKey(namespace, imageID, 1200, 400, fileID, variant.FormatWebP),
		ManifestKey(namespace, imageID),
		Prefix(namespace, imageID) + imageID.String() + "_600x400.webp",
	} {
		assert.True(t, OwnsKey(namespace, imageID, key), key)
	}

	for _, key := range []string{
		"",
		Prefix(namespace, imageID) + imageID.String() + "_",
		AssetKey(namespace, other, 1200, 400, fileID, variant.FormatWebP),
		AssetKey("other", imageID, 1200, 400, fileID, variant.FormatWebP),
		Prefix(namespace, imageID) + other.String() + "_600x400.webp",
		Prefix(namespace, imageID) + imageID.String() + "_x/../../" + other.String() + "_600x400.webp",
		Prefix(namespace, imageID) + imageID.String() + "_..webp",
		"/" + AssetKey(namespace, imageID, 1200, 400, fileID, variant.FormatWebP),
	} {
		assert.False(t, OwnsKey(namespace, imageID, key), key)
	}
}

func TestValidateID(t *testing.T) {
	t.Run("accepts the identifiers both services mint", func(t *testing.T) {
		// Every identifier that addresses an object is a v4 or v7
		for _, id := range []uuid.UUID{uuid.NewV4(), uuid.New(), imageID, siteID, uuid.NewV7()} {
			parsed, err := ValidateID(id.String())
			require.NoError(t, err)
			assert.Equal(t, id, parsed)
		}
	})

	t.Run("rejects what cannot address an object", func(t *testing.T) {
		tests := map[string]string{
			"empty":          "",
			"not a UUID":     "not-a-uuid",
			"uppercase":      strings.ToUpper(imageID.String()),
			"braced":         "{" + imageID.String() + "}",
			"urn":            "urn:uuid:" + imageID.String(),
			"no dashes":      strings.ReplaceAll(imageID.String(), "-", ""),
			"path traversal": "../../etc/passwd",
		}

		for name, id := range tests {
			t.Run(name, func(t *testing.T) {
				_, err := ValidateID(id)
				require.Error(t, err)
			})
		}
	})

	// The nil UUID parses and is canonical, but it has no version and names no image
	t.Run("rejects the nil UUID", func(t *testing.T) {
		_, err := ValidateID(uuid.Nil().String())
		require.Error(t, err)
	})
}

// A key must never escape the prefix it belongs to, whatever the identifiers are
func TestKeysStayInsideTheirPrefix(t *testing.T) {
	for range 200 {
		site, image := uuid.NewV4(), uuid.NewV4()
		for _, key := range []string{
			OriginalKey(site.String(), image, "png"),
			AssetKey(site.String(), image, 100, 100, fileID, variant.FormatWebP),
			ManifestKey(site.String(), image),
		} {
			assert.NotContains(t, key, "..")
			assert.False(t, strings.HasPrefix(key, "/"))
			assert.True(t, strings.HasPrefix(key, site.String()+"/"))
		}
	}
}

// Keys written before namespaces replaced site IDs must not move, so these are pinned to the exact strings that layout produced
// Variants are the exception: their names now end with a random file ID, and the variants stored under the older names are found through the keys their manifest records
func TestLayoutIsUnchangedForUUIDNamespaces(t *testing.T) {
	image := uuid.MustParse("01a061a9-7384-4e97-aadf-232d1e006b83")

	assert.Equal(t, "6ba7b810-9dad-41d1-80b4-00c04fd430c8/01/a0/01a061a9-7384-4e97-aadf-232d1e006b83_orig.jpg", OriginalKey(namespace, image, ".jpg"))
	assert.Equal(t, "6ba7b810-9dad-41d1-80b4-00c04fd430c8/01/a0/01a061a9-7384-4e97-aadf-232d1e006b83_1200x400_3f9a0c1e.webp", AssetKey(namespace, image, 1200, 400, fileID, variant.FormatWebP))
	assert.Equal(t, "6ba7b810-9dad-41d1-80b4-00c04fd430c8/01/a0/01a061a9-7384-4e97-aadf-232d1e006b83_manifest.json", ManifestKey(namespace, image))
}

func TestValidateNamespace(t *testing.T) {
	for _, valid := range []string{"a", "0", "default", "my-site", "tenant_42", "v1.photos", siteID.String(), strings.Repeat("a", 64)} {
		err := ValidateNamespace(valid)
		require.NoError(t, err, valid)
	}

	for _, invalid := range []string{"", "Default", "-leading", ".leading", "_leading", "with/slash", "with:colon", "with space", "a..b", "..", strings.Repeat("a", 65)} {
		err := ValidateNamespace(invalid)
		require.Error(t, err, invalid)
	}
}
