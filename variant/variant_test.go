package variant

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFormat(t *testing.T) {
	tests := []struct {
		in      string
		want    Format
		wantErr bool
	}{
		{in: "webp", want: FormatWebP},
		{in: "WEBP", want: FormatWebP},
		{in: " avif ", want: FormatAVIF},
		{in: "jpg", want: FormatJPEG},
		{in: "jpeg", want: FormatJPEG},
		{in: "png", want: FormatPNG},
		{in: "gif", wantErr: true},
		{in: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseFormat(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFormatProperties(t *testing.T) {
	assert.Equal(t, ".jpg", FormatJPEG.Extension())
	assert.Equal(t, ".webp", FormatWebP.Extension())
	assert.Equal(t, ".avif", FormatAVIF.Extension())
	assert.Equal(t, ".png", FormatPNG.Extension())

	assert.Equal(t, "image/jpeg", FormatJPEG.ContentType())
	assert.Equal(t, "image/webp", FormatWebP.ContentType())
	assert.Equal(t, "image/avif", FormatAVIF.ContentType())
	assert.Equal(t, "image/png", FormatPNG.ContentType())

	assert.True(t, FormatJPEG.SupportsQuality())
	assert.False(t, FormatPNG.SupportsQuality())
}

func TestParseFit(t *testing.T) {
	tests := []struct {
		in      string
		want    Fit
		wantErr bool
	}{
		{in: "", want: FitContain},
		{in: "contain", want: FitContain},
		{in: "COVER", want: FitCover},
		{in: "crop", want: FitCover},
		{in: "stretch", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseFit(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseResolution(t *testing.T) {
	tests := []struct {
		in       string
		want     Resolution
		wantSlug string
		wantErr  bool
	}{
		{in: "1200x400", want: Resolution{Width: 1200, Height: 400}, wantSlug: "1200x400"},
		{in: " 800X600 ", want: Resolution{Width: 800, Height: 600}, wantSlug: "800x600"},
		{in: "1200x", want: Resolution{Width: 1200}, wantSlug: "1200xauto"},
		{in: "1200", want: Resolution{Width: 1200}, wantSlug: "1200xauto"},
		{in: "x400", want: Resolution{Height: 400}, wantSlug: "autox400"},
		{in: "", wantErr: true},
		{in: "x", wantErr: true},
		{in: "0x0", wantErr: true},
		{in: "-10x20", wantErr: true},
		{in: "abcxdef", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseResolution(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantSlug, got.Slug())
		})
	}
}

func TestSpecNormalize(t *testing.T) {
	t.Run("fills in the defaults", func(t *testing.T) {
		got, err := Spec{Format: "jpg", Quality: 80, Size: Resolution{Width: 1200}}.Normalize(8192)
		require.NoError(t, err)
		assert.Equal(t, Spec{Format: FormatJPEG, Quality: 80, Size: Resolution{Width: 1200}, Fit: FitContain}, got)

		got, err = Spec{Format: FormatAVIF, Quality: 50, Size: Resolution{Width: 100, Height: 100}, Fit: "crop"}.Normalize(8192)
		require.NoError(t, err)
		assert.Equal(t, FitCover, got.Fit)
		require.NotNil(t, got.Effort)
		assert.Equal(t, DefaultAVIFEffort, *got.Effort)
	})

	t.Run("keeps an explicit zero effort", func(t *testing.T) {
		got, err := Spec{Format: FormatWebP, Quality: 80, Effort: new(0), Size: Resolution{Width: 100}}.Normalize(8192)
		require.NoError(t, err)
		require.NotNil(t, got.Effort)
		assert.Equal(t, 0, *got.Effort)
	})

	tests := map[string]Spec{
		"unknown format":          {Format: "gif", Quality: 80, Size: Resolution{Width: 100}},
		"missing quality":         {Format: FormatWebP, Size: Resolution{Width: 100}},
		"quality out of range":    {Format: FormatWebP, Quality: 101, Size: Resolution{Width: 100}},
		"quality on png":          {Format: FormatPNG, Quality: 80, Size: Resolution{Width: 100}},
		"effort on jpeg":          {Format: FormatJPEG, Quality: 80, Effort: new(3), Size: Resolution{Width: 100}},
		"effort out of range":     {Format: FormatWebP, Quality: 80, Effort: new(7), Size: Resolution{Width: 100}},
		"empty size":              {Format: FormatWebP, Quality: 80},
		"negative size":           {Format: FormatWebP, Quality: 80, Size: Resolution{Width: -1, Height: 10}},
		"cover with one side":     {Format: FormatWebP, Quality: 80, Size: Resolution{Width: 100}, Fit: FitCover},
		"larger than the maximum": {Format: FormatWebP, Quality: 80, Size: Resolution{Width: 9000}},
		"unknown fit":             {Format: FormatWebP, Quality: 80, Size: Resolution{Width: 100}, Fit: "stretch"},
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := spec.Normalize(8192)
			require.Error(t, err)
		})
	}
}

func TestNormalizeSet(t *testing.T) {
	webp := Spec{Format: FormatWebP, Quality: 80, Size: Resolution{Width: 100, Height: 100}}

	t.Run("drops exact duplicates, including ones that differ only by an omitted default", func(t *testing.T) {
		got, err := NormalizeSet([]Spec{webp, {Format: "WEBP", Quality: 80, Effort: new(DefaultWebPEffort), Size: Resolution{Width: 100, Height: 100}, Fit: FitContain}}, 8192, 10)
		require.NoError(t, err)
		require.Len(t, got, 1)
	})

	t.Run("keeps the same box with another fit", func(t *testing.T) {
		cover := webp
		cover.Fit = FitCover
		got, err := NormalizeSet([]Spec{webp, cover}, 8192, 10)
		require.NoError(t, err)
		require.Len(t, got, 2)
	})

	// Every quality is a file of its own, so one box can be asked for at several
	t.Run("keeps one box at several qualities", func(t *testing.T) {
		other := webp
		other.Quality = 60
		got, err := NormalizeSet([]Spec{webp, other}, 8192, 10)
		require.NoError(t, err)
		require.Len(t, got, 2)
	})

	// The effort doesn't change the picture, so two specs that differ only by it would be served by one file
	t.Run("refuses specs that differ only by their effort", func(t *testing.T) {
		other := webp
		other.Effort = new(2)
		_, err := NormalizeSet([]Spec{webp, other}, 8192, 10)
		require.ErrorContains(t, err, "conflicts")
	})

	t.Run("enforces the maximum count after deduplication", func(t *testing.T) {
		second := webp
		second.Size = Resolution{Width: 200}
		_, err := NormalizeSet([]Spec{webp, webp, second}, 8192, 1)
		require.ErrorContains(t, err, "maximum of 1")

		got, err := NormalizeSet([]Spec{webp, webp}, 8192, 1)
		require.NoError(t, err)
		require.Len(t, got, 1)
	})
}
