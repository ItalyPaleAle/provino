package actors

import (
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/imagekeys"
	"github.com/italypaleale/provino/internal/imaging"
	"github.com/italypaleale/provino/manifest"
)

// Build a variant specification for a box
func planSpec(format imaging.Format, width int, height int, fit imaging.Fit) VariantSpec {
	return VariantSpec{Format: format, Quality: 80, Resolution: imaging.Resolution{Width: width, Height: height}, Fit: fit}
}

func TestPlanSkips(t *testing.T) {
	t.Run("only the tightest box the source can't fill stores the file", func(t *testing.T) {
		// The design's example: an 800x800 original in 1000x1000 and 2000x2000 boxes gets one 800x800 file
		skips := planSkips(imaging.Resolution{Width: 800, Height: 800}, []VariantSpec{
			planSpec(imaging.FormatWebP, 1000, 1000, imaging.FitContain),
			planSpec(imaging.FormatWebP, 2000, 2000, imaging.FitContain),
		})
		assert.Nil(t, skips[0])
		assert.Equal(t, &SkippedVariant{Index: 0, For: imaging.Resolution{Width: 1000, Height: 1000}}, skips[1])
	})

	t.Run("the holder is the tightest box wherever it's listed", func(t *testing.T) {
		skips := planSkips(imaging.Resolution{Width: 800, Height: 800}, []VariantSpec{
			planSpec(imaging.FormatWebP, 2000, 2000, imaging.FitContain),
			planSpec(imaging.FormatWebP, 1000, 1000, imaging.FitContain),
		})
		assert.Equal(t, &SkippedVariant{Index: 1, For: imaging.Resolution{Width: 1000, Height: 1000}}, skips[0])
		assert.Nil(t, skips[1])
	})

	t.Run("gallery profile on a small scan", func(t *testing.T) {
		// 500x500 and 900x1350 are filled, 1400x1400 holds the 1200x800 original, and 2048x2048 is skipped, in each format
		specs := make([]VariantSpec, 0, 8)
		for _, format := range []imaging.Format{imaging.FormatAVIF, imaging.FormatWebP} {
			specs = append(specs,
				planSpec(format, 500, 500, imaging.FitContain),
				planSpec(format, 900, 1350, imaging.FitContain),
				planSpec(format, 1400, 1400, imaging.FitContain),
				planSpec(format, 2048, 2048, imaging.FitContain),
			)
		}
		skips := planSkips(imaging.Resolution{Width: 1200, Height: 800}, specs)
		assert.Equal(t, []*SkippedVariant{
			nil, nil, nil, {Index: 2, For: imaging.Resolution{Width: 1400, Height: 1400}},
			nil, nil, nil, {Index: 6, For: imaging.Resolution{Width: 1400, Height: 1400}},
		}, skips)
	})

	t.Run("cover boxes of one shape share the crop", func(t *testing.T) {
		skips := planSkips(imaging.Resolution{Width: 800, Height: 800}, []VariantSpec{
			planSpec(imaging.FormatWebP, 1200, 500, imaging.FitCover),
			planSpec(imaging.FormatWebP, 2400, 1000, imaging.FitCover),
		})
		assert.Nil(t, skips[0])
		assert.Equal(t, &SkippedVariant{Index: 0, For: imaging.Resolution{Width: 1200, Height: 500}}, skips[1])
	})

	t.Run("cover boxes of different shapes are different crops", func(t *testing.T) {
		skips := planSkips(imaging.Resolution{Width: 800, Height: 800}, []VariantSpec{
			planSpec(imaging.FormatWebP, 1200, 500, imaging.FitCover),
			planSpec(imaging.FormatWebP, 1000, 1000, imaging.FitCover),
		})
		assert.Equal(t, []*SkippedVariant{nil, nil}, skips)
	})

	t.Run("fits and formats never share a file", func(t *testing.T) {
		// The contain box and the square cover box would both come out at 800x800, but they're compared per fit
		skips := planSkips(imaging.Resolution{Width: 800, Height: 800}, []VariantSpec{
			planSpec(imaging.FormatWebP, 1000, 1000, imaging.FitContain),
			planSpec(imaging.FormatWebP, 1000, 1000, imaging.FitCover),
			planSpec(imaging.FormatAVIF, 2000, 2000, imaging.FitContain),
		})
		assert.Equal(t, []*SkippedVariant{nil, nil, nil}, skips)
	})

	t.Run("qualities never share a file", func(t *testing.T) {
		low := planSpec(imaging.FormatWebP, 2000, 2000, imaging.FitContain)
		low.Quality = 40
		skips := planSkips(imaging.Resolution{Width: 800, Height: 800}, []VariantSpec{
			planSpec(imaging.FormatWebP, 1000, 1000, imaging.FitContain),
			low,
		})
		assert.Equal(t, []*SkippedVariant{nil, nil}, skips)
	})

	t.Run("an empty fit reads as contain", func(t *testing.T) {
		skips := planSkips(imaging.Resolution{Width: 800, Height: 800}, []VariantSpec{
			planSpec(imaging.FormatWebP, 1000, 1000, imaging.FitContain),
			planSpec(imaging.FormatWebP, 2000, 2000, ""),
		})
		assert.Equal(t, &SkippedVariant{Index: 0, For: imaging.Resolution{Width: 1000, Height: 1000}}, skips[1])
	})

	t.Run("boxes the source fills are never skipped", func(t *testing.T) {
		skips := planSkips(imaging.Resolution{Width: 4000, Height: 3000}, []VariantSpec{
			planSpec(imaging.FormatWebP, 1000, 1000, imaging.FitContain),
			planSpec(imaging.FormatWebP, 2000, 2000, imaging.FitContain),
		})
		assert.Equal(t, []*SkippedVariant{nil, nil}, skips)
	})

	t.Run("an unknown source size skips nothing", func(t *testing.T) {
		skips := planSkips(imaging.Resolution{}, []VariantSpec{
			planSpec(imaging.FormatWebP, 1000, 1000, imaging.FitContain),
			planSpec(imaging.FormatWebP, 2000, 2000, imaging.FitContain),
		})
		assert.Equal(t, []*SkippedVariant{nil, nil}, skips)
	})
}

func TestBuildManifestRecordsSkippedVariants(t *testing.T) {
	imageID := uuid.MustParse("01a061a9-7384-4e97-aadf-232d1e006b83")
	req := JobRequest{JobID: uuid.NewV7().String(), Namespace: "default", ImageID: imageID.String(), Variants: []VariantSpec{
		{Format: imaging.FormatWebP, Quality: 80, Effort: 5, Resolution: imaging.Resolution{Width: 1000, Height: 1000}, Fit: imaging.FitContain},
		{Format: imaging.FormatWebP, Quality: 80, Effort: 5, Resolution: imaging.Resolution{Width: 2000, Height: 2000}, Fit: imaging.FitContain},
	}}
	holderKey := "default/01/a0/01a061a9-7384-4e97-aadf-232d1e006b83_800x800_3f9a0c1e.webp"
	now := time.Now()
	snap := jobSnapshot{SourceFormat: "png", SourceSize: 999, StartedAt: now}
	results := []VariantResult{
		{Key: holderKey, Width: 800, Height: 800, Size: 1234, CompletedAt: now},
		{CompletedAt: now, Skipped: &SkippedVariant{Index: 0, For: imaging.Resolution{Width: 1000, Height: 1000}}},
	}

	m, err := buildManifest(req, snap, imaging.Resolution{Width: 800, Height: 800}, results)
	require.NoError(t, err)
	assert.Equal(t, manifest.Version, m.Version)
	assert.Equal(t, manifest.StatusCompleted, m.Status)
	assert.Equal(t, req.JobID, m.JobID)
	assert.Equal(t, "default", m.Namespace)
	assert.Equal(t, 800, m.SourceWidth)
	assert.Equal(t, 800, m.SourceHeight)
	assert.Equal(t, "png", m.SourceFormat)
	assert.Equal(t, "default/01/a0/01a061a9-7384-4e97-aadf-232d1e006b83_orig.png", m.SourceKey)

	// An encoded variant is listed at the key it was stored at
	holder := m.Variants[0]
	assert.False(t, holder.Skipped)
	assert.Nil(t, holder.SkippedFor)
	assert.Equal(t, holderKey, holder.Key)

	// The skipped entry repeats the holder's file, key included, so a reader that ignores skipped sees a duplicate it drops
	skipped := m.Variants[1]
	assert.True(t, skipped.Skipped)
	assert.Equal(t, &imaging.Resolution{Width: 1000, Height: 1000}, skipped.SkippedFor)
	assert.Equal(t, imaging.Resolution{Width: 2000, Height: 2000}, skipped.Resolution)
	assert.Equal(t, holder.Key, skipped.Key)
	assert.Equal(t, holder.Width, skipped.Width)
	assert.Equal(t, holder.Height, skipped.Height)
	assert.Equal(t, holder.Size, skipped.Size)

	t.Run("a skip pointing at another skipped variant is rejected", func(t *testing.T) {
		broken := []VariantResult{
			{Skipped: &SkippedVariant{Index: 1}},
			{Skipped: &SkippedVariant{Index: 0}},
		}
		_, err := buildManifest(req, snap, imaging.Resolution{}, broken)
		require.ErrorContains(t, err, "doesn't hold a file")
	})

	t.Run("an encoded variant without a key is rejected", func(t *testing.T) {
		broken := []VariantResult{{Width: 800, Height: 800, Size: 1234}, results[1]}
		_, err := buildManifest(req, snap, imaging.Resolution{}, broken)
		require.ErrorContains(t, err, "records no key")
	})

	// A reused file keeps the key it's stored at, which may be a name an older layout gave it
	t.Run("a reused variant keeps its stored key", func(t *testing.T) {
		stored := manifest.Variant{Key: "default/01/a0/01a061a9-7384-4e97-aadf-232d1e006b83_800x800.webp", Format: imaging.FormatWebP, Quality: 80, Effort: 4, Resolution: imaging.Resolution{Width: 1000, Height: 1000}, Fit: imaging.FitContain, Width: 800, Height: 800, Size: 1234}
		reused := []VariantResult{
			{Width: 800, Height: 800, Size: 1234, CompletedAt: now, Reused: &stored},
			results[1],
		}
		m, err := buildManifest(req, snap, imaging.Resolution{Width: 800, Height: 800}, reused)
		require.NoError(t, err)
		assert.Equal(t, stored.Key, m.Variants[0].Key)
		assert.Equal(t, 4, m.Variants[0].Effort)
		assert.Equal(t, stored.Key, m.Variants[1].Key)
	})

	t.Run("a skip pointing outside the job is rejected", func(t *testing.T) {
		broken := []VariantResult{results[0], {Skipped: &SkippedVariant{Index: 7}}}
		_, err := buildManifest(req, snap, imaging.Resolution{}, broken)
		require.ErrorContains(t, err, "doesn't hold a file")
	})
}

func TestAssignFileIDs(t *testing.T) {
	imageID := uuid.MustParse("01a061a9-7384-4e97-aadf-232d1e006b83")
	existing := manifest.Variant{Key: imagekeys.AssetKey("default", imageID, 800, 600, "00000002", imaging.FormatWebP)}
	previous := &manifest.Manifest{Variants: []manifest.Variant{
		existing,
		// A name an older layout gave a file has no file ID to avoid
		{Key: imagekeys.Prefix("default", imageID) + imageID.String() + "_400x300.webp"},
	}}
	tasks := []variantTask{
		{},
		{Existing: &existing},
		{Skip: &SkippedVariant{Index: 0}},
		{},
		{},
	}

	// The generator hands out an ID the previous manifest lists, and then one the job already gave out, and both are drawn again
	ids := []string{"00000001", "00000002", "00000003", "00000001", "00000003", "00000004"}
	next := 0
	assignFileIDs(tasks, previous, func() string {
		id := ids[next]
		next++
		return id
	})

	assert.Equal(t, "00000001", tasks[0].FileID)
	assert.Empty(t, tasks[1].FileID, "a reused variant stores nothing")
	assert.Empty(t, tasks[2].FileID, "a skipped variant stores nothing")
	assert.Equal(t, "00000003", tasks[3].FileID)
	assert.Equal(t, "00000004", tasks[4].FileID)
	assert.Equal(t, len(ids), next)
}
