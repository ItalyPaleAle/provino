package imaging

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFullResolution(t *testing.T) {
	tests := []struct {
		name     string
		source   Resolution
		box      Resolution
		fit      Fit
		wantSize Resolution
		wantFull bool
	}{
		{
			name:   "contain box the source fills",
			source: Resolution{Width: 1200, Height: 800},
			box:    Resolution{Width: 900, Height: 1350},
			fit:    FitContain,
		},
		{
			name:     "contain box larger than the source",
			source:   Resolution{Width: 800, Height: 800},
			box:      Resolution{Width: 1000, Height: 1000},
			fit:      FitContain,
			wantSize: Resolution{Width: 800, Height: 800},
			wantFull: true,
		},
		{
			// The landscape source touches the box on its width, so its variant isn't scaled at all
			name:     "contain box the source matches on its binding side",
			source:   Resolution{Width: 1400, Height: 787},
			box:      Resolution{Width: 1400, Height: 1400},
			fit:      FitContain,
			wantSize: Resolution{Width: 1400, Height: 787},
			wantFull: true,
		},
		{
			name:     "contain box larger on one side only is still filled",
			source:   Resolution{Width: 1200, Height: 800},
			box:      Resolution{Width: 2000, Height: 600},
			fit:      FitContain,
			wantFull: false,
		},
		{
			name:     "contain width only, wider than the source",
			source:   Resolution{Width: 1200, Height: 800},
			box:      Resolution{Width: 2048},
			fit:      FitContain,
			wantSize: Resolution{Width: 1200, Height: 800},
			wantFull: true,
		},
		{
			name:   "contain height only, shorter than the source",
			source: Resolution{Width: 1200, Height: 800},
			box:    Resolution{Height: 400},
			fit:    FitContain,
		},
		{
			name:     "empty fit reads as contain",
			source:   Resolution{Width: 800, Height: 800},
			box:      Resolution{Width: 1000, Height: 1000},
			wantSize: Resolution{Width: 800, Height: 800},
			wantFull: true,
		},
		{
			name:   "cover box the source fills",
			source: Resolution{Width: 2400, Height: 1600},
			box:    Resolution{Width: 1200, Height: 500},
			fit:    FitCover,
		},
		{
			// The design's example: the crop keeps the box's 12:5 shape at the source's full width
			name:     "cover box wider than the source",
			source:   Resolution{Width: 800, Height: 800},
			box:      Resolution{Width: 1200, Height: 500},
			fit:      FitCover,
			wantSize: Resolution{Width: 800, Height: 333},
			wantFull: true,
		},
		{
			name:     "cover box taller than the source",
			source:   Resolution{Width: 1200, Height: 800},
			box:      Resolution{Width: 900, Height: 1350},
			fit:      FitCover,
			wantSize: Resolution{Width: 533, Height: 800},
			wantFull: true,
		},
		{
			name:     "cover box the source matches on one side",
			source:   Resolution{Width: 1200, Height: 800},
			box:      Resolution{Width: 1200, Height: 500},
			fit:      FitCover,
			wantSize: Resolution{Width: 1200, Height: 500},
			wantFull: true,
		},
		{
			name:   "cover needs both sides",
			source: Resolution{Width: 800, Height: 800},
			box:    Resolution{Width: 1200},
			fit:    FitCover,
		},
		{
			name:   "unknown source size",
			source: Resolution{},
			box:    Resolution{Width: 1000, Height: 1000},
			fit:    FitContain,
		},
		{
			name:   "empty box",
			source: Resolution{Width: 800, Height: 800},
			fit:    FitContain,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size, full := FullResolution(tt.source, tt.box, tt.fit)
			assert.Equal(t, tt.wantFull, full)
			assert.Equal(t, tt.wantSize, size)
		})
	}
}

func TestScaleFactor(t *testing.T) {
	source := Resolution{Width: 1200, Height: 800}

	// Contain fits inside the box, so the tighter side decides
	assert.InDelta(t, 0.75, ScaleFactor(source, Resolution{Width: 900, Height: 1350}, FitContain), 1e-9)
	assert.InDelta(t, 1400.0/1200, ScaleFactor(source, Resolution{Width: 1400, Height: 1400}, FitContain), 1e-9)
	assert.InDelta(t, 2048.0/1200, ScaleFactor(source, Resolution{Width: 2048}, FitContain), 1e-9)

	// Cover fills the box, so the looser side decides
	assert.InDelta(t, 1.0, ScaleFactor(source, Resolution{Width: 1200, Height: 500}, FitCover), 1e-9)
	assert.InDelta(t, 2.0, ScaleFactor(source, Resolution{Width: 2400, Height: 1000}, FitCover), 1e-9)

	assert.True(t, math.IsInf(ScaleFactor(Resolution{}, Resolution{Width: 100, Height: 100}, FitContain), 1))
}
