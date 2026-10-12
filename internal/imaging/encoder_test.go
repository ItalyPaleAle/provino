package imaging

import (
	"context"
	"testing"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Generate a PNG to use as the source image of the tests, sized so variants are always a downscale
func testImage(t *testing.T, width int, height int) []byte {
	t.Helper()

	img, err := vips.Black(width, height)
	require.NoError(t, err)
	defer img.Close()

	buf, _, err := img.ExportPng(vips.NewPngExportParams())
	require.NoError(t, err)

	return buf
}

func newTestEncoder(t *testing.T) *Encoder {
	t.Helper()

	err := Startup(t.Context(), Options{})
	require.NoError(t, err)

	return NewEncoder()
}

func TestEncoderGenerate(t *testing.T) {
	encoder := newTestEncoder(t)
	source := testImage(t, 1600, 1200)

	tests := []struct {
		name       string
		format     Format
		resolution Resolution
		fit        Fit
		wantWidth  int
		wantHeight int
	}{
		{
			name:       "webp contain",
			format:     FormatWebP,
			resolution: Resolution{Width: 400, Height: 400},
			fit:        FitContain,
			// The source is 4:3, so fitting it in a square box makes the height the binding side
			wantWidth:  400,
			wantHeight: 300,
		},
		{
			name:       "avif cover",
			format:     FormatAVIF,
			resolution: Resolution{Width: 200, Height: 200},
			fit:        FitCover,
			wantWidth:  200,
			wantHeight: 200,
		},
		{
			name:       "jpeg width only",
			format:     FormatJPEG,
			resolution: Resolution{Width: 800},
			fit:        FitContain,
			wantWidth:  800,
			wantHeight: 600,
		},
		{
			name:       "png height only",
			format:     FormatPNG,
			resolution: Resolution{Height: 300},
			fit:        FitContain,
			wantWidth:  400,
			wantHeight: 300,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := encoder.Generate(t.Context(), Request{
				Source:     source,
				Format:     tt.format,
				Quality:    60,
				Resolution: tt.resolution,
				Fit:        tt.fit,
			})
			require.NoError(t, err)

			assert.Equal(t, tt.wantWidth, res.Width)
			assert.Equal(t, tt.wantHeight, res.Height)
			assert.Positive(t, res.Size)
			assert.Len(t, res.Data, int(res.Size))

			// The encoded variant must actually be in the requested format, which is what a client relies on when it picks a variant by its extension
			written, err := vips.NewImageFromBuffer(res.Data)
			require.NoError(t, err)
			defer written.Close()
			assert.Equal(t, tt.wantWidth, written.Width())
			assert.Equal(t, tt.wantHeight, written.Height())
		})
	}
}

// Encode a JPEG whose pixels are stored at one size and whose EXIF orientation turns them by a quarter
func testRotatedJPEG(t *testing.T, storedWidth int, storedHeight int) []byte {
	t.Helper()

	img, err := vips.Black(storedWidth, storedHeight)
	require.NoError(t, err)
	defer img.Close()
	err = img.ToColorSpace(vips.InterpretationSRGB)
	require.NoError(t, err)
	err = img.SetOrientation(6)
	require.NoError(t, err)

	params := vips.NewJpegExportParams()
	params.Quality = 80
	buf, _, err := img.ExportJpeg(params)
	require.NoError(t, err)

	return buf
}

// A box the source can't fill gets the source's own pixels, so no variant is ever larger than its source
func TestEncoderGenerateNeverEnlarges(t *testing.T) {
	encoder := newTestEncoder(t)
	square := testImage(t, 800, 800)
	landscape := testImage(t, 1200, 800)

	tests := []struct {
		name       string
		source     []byte
		resolution Resolution
		fit        Fit
		wantWidth  int
		wantHeight int
	}{
		{
			name:       "contain box larger than the source",
			source:     square,
			resolution: Resolution{Width: 1000, Height: 1000},
			fit:        FitContain,
			wantWidth:  800,
			wantHeight: 800,
		},
		{
			name:       "contain width only, wider than the source",
			source:     square,
			resolution: Resolution{Width: 2000},
			fit:        FitContain,
			wantWidth:  800,
			wantHeight: 800,
		},
		{
			// Shrinking only would give 800x500 here, which is a different shape than the box
			name:       "cover box wider than the source keeps the box's shape",
			source:     square,
			resolution: Resolution{Width: 1200, Height: 500},
			fit:        FitCover,
			wantWidth:  800,
			wantHeight: 333,
		},
		{
			name:       "cover box taller than the source keeps the box's shape",
			source:     landscape,
			resolution: Resolution{Width: 900, Height: 1350},
			fit:        FitCover,
			wantWidth:  533,
			wantHeight: 800,
		},
		{
			name:       "cover box the source fills is unchanged",
			source:     landscape,
			resolution: Resolution{Width: 600, Height: 250},
			fit:        FitCover,
			wantWidth:  600,
			wantHeight: 250,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := encoder.Generate(t.Context(), Request{
				Source:     tt.source,
				Format:     FormatWebP,
				Quality:    80,
				Resolution: tt.resolution,
				Fit:        tt.fit,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.wantWidth, res.Width)
			assert.Equal(t, tt.wantHeight, res.Height)
		})
	}

	// The crop is computed on the source as it's displayed, after its orientation turns it upright
	t.Run("cover box on a rotated source", func(t *testing.T) {
		res, err := encoder.Generate(t.Context(), Request{
			Source:     testRotatedJPEG(t, 800, 600),
			Format:     FormatWebP,
			Quality:    80,
			Resolution: Resolution{Width: 1200, Height: 500},
			Fit:        FitCover,
		})
		require.NoError(t, err)
		assert.Equal(t, 600, res.Width)
		assert.Equal(t, 250, res.Height)
	})
}

func TestEncoderSourceSize(t *testing.T) {
	encoder := newTestEncoder(t)

	t.Run("plain source", func(t *testing.T) {
		size, err := encoder.SourceSize(t.Context(), testImage(t, 640, 480))
		require.NoError(t, err)
		assert.Equal(t, Resolution{Width: 640, Height: 480}, size)
	})

	// Variants apply the EXIF orientation, so the size they see is the upright one
	t.Run("rotated source", func(t *testing.T) {
		size, err := encoder.SourceSize(t.Context(), testRotatedJPEG(t, 800, 600))
		require.NoError(t, err)
		assert.Equal(t, Resolution{Width: 600, Height: 800}, size)
	})

	t.Run("empty source", func(t *testing.T) {
		_, err := encoder.SourceSize(t.Context(), nil)
		require.ErrorIs(t, err, ErrInvalidRequest)
	})

	t.Run("unreadable source", func(t *testing.T) {
		_, err := encoder.SourceSize(t.Context(), []byte("hello world, this is not an image"))
		require.ErrorIs(t, err, ErrSourceUnreadable)
	})
}

// A variant generated by a retried job must produce the same result
func TestEncoderGenerateIsRepeatable(t *testing.T) {
	encoder := newTestEncoder(t)

	req := Request{
		Source:     testImage(t, 800, 600),
		Format:     FormatWebP,
		Quality:    80,
		Effort:     DefaultWebPEffort,
		Resolution: Resolution{Width: 100, Height: 100},
		Fit:        FitContain,
	}

	first, err := encoder.Generate(t.Context(), req)
	require.NoError(t, err)
	second, err := encoder.Generate(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestEncoderGenerateErrors(t *testing.T) {
	encoder := newTestEncoder(t)
	source := testImage(t, 400, 300)

	t.Run("empty resolution is not retryable", func(t *testing.T) {
		_, err := encoder.Generate(t.Context(), Request{
			Source:     source,
			Format:     FormatWebP,
			Quality:    80,
			Resolution: Resolution{},
		})
		require.ErrorIs(t, err, ErrInvalidRequest)
	})

	t.Run("cover requires both sides", func(t *testing.T) {
		_, err := encoder.Generate(t.Context(), Request{
			Source:     source,
			Format:     FormatWebP,
			Quality:    80,
			Resolution: Resolution{Width: 100},
			Fit:        FitCover,
		})
		require.ErrorIs(t, err, ErrInvalidRequest)
	})

	// Lossy formats carry their own quality, so there's nothing to fall back on when a request doesn't have one
	t.Run("lossy format without a quality", func(t *testing.T) {
		_, err := encoder.Generate(t.Context(), Request{
			Source:     source,
			Format:     FormatWebP,
			Resolution: Resolution{Width: 100, Height: 100},
		})
		require.ErrorIs(t, err, ErrInvalidRequest)
	})

	t.Run("quality out of range", func(t *testing.T) {
		_, err := encoder.Generate(t.Context(), Request{
			Source:     source,
			Format:     FormatWebP,
			Quality:    101,
			Resolution: Resolution{Width: 100, Height: 100},
		})
		require.ErrorIs(t, err, ErrInvalidRequest)
	})

	t.Run("effort out of range", func(t *testing.T) {
		_, err := encoder.Generate(t.Context(), Request{
			Source:     source,
			Format:     FormatWebP,
			Quality:    80,
			Effort:     7,
			Resolution: Resolution{Width: 100, Height: 100},
		})
		require.ErrorIs(t, err, ErrInvalidRequest)
	})

	t.Run("effort on unsupported format", func(t *testing.T) {
		_, err := encoder.Generate(t.Context(), Request{
			Source:     source,
			Format:     FormatJPEG,
			Quality:    80,
			Effort:     1,
			Resolution: Resolution{Width: 100, Height: 100},
		})
		require.ErrorIs(t, err, ErrInvalidRequest)
	})

	// PNG is lossless, so it's the one format that doesn't need a quality
	t.Run("lossless format without a quality", func(t *testing.T) {
		_, err := encoder.Generate(t.Context(), Request{
			Source:     source,
			Format:     FormatPNG,
			Resolution: Resolution{Width: 100, Height: 100},
		})
		require.NoError(t, err)
	})

	t.Run("unsupported format", func(t *testing.T) {
		_, err := encoder.Generate(t.Context(), Request{
			Source:     source,
			Format:     Format("tiff"),
			Quality:    80,
			Resolution: Resolution{Width: 100, Height: 100},
		})
		require.ErrorIs(t, err, ErrInvalidRequest)
	})

	t.Run("empty source", func(t *testing.T) {
		_, err := encoder.Generate(t.Context(), Request{
			Format:     FormatWebP,
			Quality:    80,
			Resolution: Resolution{Width: 100, Height: 100},
		})
		require.ErrorIs(t, err, ErrInvalidRequest)
	})

	t.Run("unreadable source is not retryable", func(t *testing.T) {
		_, err := encoder.Generate(t.Context(), Request{
			Source:     []byte("hello world"),
			Format:     FormatWebP,
			Quality:    80,
			Resolution: Resolution{Width: 100, Height: 100},
		})
		require.ErrorIs(t, err, ErrSourceUnreadable)
	})

	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := encoder.Generate(ctx, Request{
			Source:     source,
			Format:     FormatWebP,
			Quality:    80,
			Resolution: Resolution{Width: 100, Height: 100},
		})
		require.ErrorIs(t, err, context.Canceled)
	})
}
