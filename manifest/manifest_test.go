package manifest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/variant"
)

func TestParse(t *testing.T) {
	t.Run("round-trips a manifest", func(t *testing.T) {
		original := Manifest{
			Version:      Version,
			Namespace:    "default",
			ImageID:      "0b8cb0f7-2a1a-4f0f-9a9e-2b3a3f2f5f2a",
			Status:       StatusCompleted,
			JobID:        "019a7c3e-5b2d-7f41-9c8a-2d4e6f8a0b1c",
			SourceKey:    "default/0b/8c/0b8cb0f7-2a1a-4f0f-9a9e-2b3a3f2f5f2a_orig.jpg",
			SourceFormat: "jpg",
			SourceWidth:  2400,
			SourceHeight: 1600,
			StoredAt:     time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC),
			Variants: []Variant{{
				Key:        "default/0b/8c/0b8cb0f7-2a1a-4f0f-9a9e-2b3a3f2f5f2a_600x400.webp",
				Format:     variant.FormatWebP,
				Quality:    80,
				Effort:     5,
				Resolution: variant.Resolution{Width: 1200, Height: 400},
				Fit:        variant.FitContain,
				Width:      600,
				Height:     400,
				Size:       38122,
			}},
		}
		data, err := json.Marshal(original)
		require.NoError(t, err)

		parsed, err := Parse(data)
		require.NoError(t, err)
		assert.Equal(t, original, parsed)
	})

	t.Run("refuses a manifest without a version", func(t *testing.T) {
		// This is the shape manifests had before provino/v1
		_, err := Parse([]byte(`{"imageId":"x","siteId":"y","status":"completed","thumbnails":[]}`))
		require.ErrorIs(t, err, ErrUnsupportedVersion)
	})

	t.Run("refuses another version", func(t *testing.T) {
		_, err := Parse([]byte(`{"version":"provino/v2","namespace":"default"}`))
		require.ErrorIs(t, err, ErrUnsupportedVersion)
	})

	t.Run("refuses malformed JSON", func(t *testing.T) {
		_, err := Parse([]byte(`{"version":`))
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrUnsupportedVersion)
	})
}
