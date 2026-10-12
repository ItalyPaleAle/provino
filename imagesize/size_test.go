package imagesize

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFullResolution(t *testing.T) {
	tests := []struct {
		name     string
		source   Size
		box      Size
		cover    bool
		wantSize Size
		wantFull bool
	}{
		{
			name:   "contain box the source fills",
			source: Size{Width: 1200, Height: 800},
			box:    Size{Width: 900, Height: 1350},
		},
		{
			name:     "contain box larger than the source",
			source:   Size{Width: 800, Height: 800},
			box:      Size{Width: 1000, Height: 1000},
			wantSize: Size{Width: 800, Height: 800},
			wantFull: true,
		},
		{
			// The landscape source touches the box on its width, so its variant isn't scaled at all
			name:     "contain box the source matches on its binding side",
			source:   Size{Width: 1400, Height: 787},
			box:      Size{Width: 1400, Height: 1400},
			wantSize: Size{Width: 1400, Height: 787},
			wantFull: true,
		},
		{
			name:     "contain box larger on one side only is still filled",
			source:   Size{Width: 1200, Height: 800},
			box:      Size{Width: 2000, Height: 600},
			wantFull: false,
		},
		{
			name:     "contain width only, wider than the source",
			source:   Size{Width: 1200, Height: 800},
			box:      Size{Width: 2048},
			wantSize: Size{Width: 1200, Height: 800},
			wantFull: true,
		},
		{
			name:   "contain height only, shorter than the source",
			source: Size{Width: 1200, Height: 800},
			box:    Size{Height: 400},
		},
		{
			name:   "cover box the source fills",
			source: Size{Width: 2400, Height: 1600},
			box:    Size{Width: 1200, Height: 500},
			cover:  true,
		},
		{
			// The design's example: the crop keeps the box's 12:5 shape at the source's full width
			name:     "cover box wider than the source",
			source:   Size{Width: 800, Height: 800},
			box:      Size{Width: 1200, Height: 500},
			cover:    true,
			wantSize: Size{Width: 800, Height: 333},
			wantFull: true,
		},
		{
			name:     "cover box taller than the source",
			source:   Size{Width: 1200, Height: 800},
			box:      Size{Width: 900, Height: 1350},
			cover:    true,
			wantSize: Size{Width: 533, Height: 800},
			wantFull: true,
		},
		{
			name:     "cover box the source matches on one side",
			source:   Size{Width: 1200, Height: 800},
			box:      Size{Width: 1200, Height: 500},
			cover:    true,
			wantSize: Size{Width: 1200, Height: 500},
			wantFull: true,
		},
		{
			name:   "cover needs both sides",
			source: Size{Width: 800, Height: 800},
			box:    Size{Width: 1200},
			cover:  true,
		},
		{
			name:   "unknown source size",
			source: Size{},
			box:    Size{Width: 1000, Height: 1000},
		},
		{
			name:   "empty box",
			source: Size{Width: 800, Height: 800},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size, full := FullResolution(tt.source, tt.box, tt.cover)
			assert.Equal(t, tt.wantFull, full)
			assert.Equal(t, tt.wantSize, size)
		})
	}
}

func TestScaleFactor(t *testing.T) {
	source := Size{Width: 1200, Height: 800}

	// Contain fits inside the box, so the tighter side decides
	assert.InDelta(t, 0.75, ScaleFactor(source, Size{Width: 900, Height: 1350}, false), 1e-9)
	assert.InDelta(t, 1400.0/1200, ScaleFactor(source, Size{Width: 1400, Height: 1400}, false), 1e-9)
	assert.InDelta(t, 2048.0/1200, ScaleFactor(source, Size{Width: 2048}, false), 1e-9)

	// Cover fills the box, so the looser side decides
	assert.InDelta(t, 1.0, ScaleFactor(source, Size{Width: 1200, Height: 500}, true), 1e-9)
	assert.InDelta(t, 2.0, ScaleFactor(source, Size{Width: 2400, Height: 1000}, true), 1e-9)

	assert.True(t, math.IsInf(ScaleFactor(Size{}, Size{Width: 100, Height: 100}, false), 1))
}
