// Package imagesize holds the variant geometry provino and the applications that plan variant sets share, so both agree on which boxes a source can fill
package imagesize

import (
	"math"
)

// Size is a width and a height in pixels
// A zero side of a box means that side is unconstrained
type Size struct {
	Width  int
	Height int
}

// NewSize returns a new Size object
func NewSize(width int, height int) Size {
	return Size{
		Width:  width,
		Height: height,
	}
}

// FullResolution reports whether a source can't fill a box, so the variant for that box keeps the source's own pixels instead of scaling them up
// When it can't, the returned size is that variant's: the whole source with contain, or the largest centered crop of the box's shape with cover
// A source exactly as large as the box on its binding side counts as one that can't fill it, because its variant isn't scaled either
func FullResolution(source Size, box Size, cover bool) (Size, bool) {
	if source.Width <= 0 || source.Height <= 0 || (box.Width <= 0 && box.Height <= 0) {
		return Size{}, false
	}

	if !cover {
		// Contain scales by its tightest side, so the source has to shrink as soon as it's larger than the box on any side the box constrains
		if (box.Width > 0 && source.Width > box.Width) || (box.Height > 0 && source.Height > box.Height) {
			return Size{}, false
		}
		return source, true
	}

	// Cover scales by its loosest side, so the source has to shrink only when it's larger than the box on both sides
	if box.Width <= 0 || box.Height <= 0 || (source.Width > box.Width && source.Height > box.Height) {
		return Size{}, false
	}

	// The largest crop of the box's shape keeps one side of the source whole and trims the other
	if source.Width*box.Height >= source.Height*box.Width {
		// The source is at least as wide as the box's shape, so it keeps its full height
		width := clampSide(roundDiv(source.Height*box.Width, box.Height), source.Width)
		return NewSize(width, source.Height), true
	}

	height := clampSide(roundDiv(source.Width*box.Height, box.Width), source.Height)
	return NewSize(source.Width, height), true
}

// ScaleFactor returns how much a variant for a box would scale a source if it were allowed to enlarge it
// Below 1 the variant shrinks the source, and among the boxes a source can't fill, the one with the smallest factor is the tightest
func ScaleFactor(source Size, box Size, cover bool) float64 {
	if source.Width <= 0 || source.Height <= 0 {
		return math.Inf(1)
	}

	horizontal := math.Inf(1)
	if box.Width > 0 {
		horizontal = float64(box.Width) / float64(source.Width)
	}
	vertical := math.Inf(1)
	if box.Height > 0 {
		vertical = float64(box.Height) / float64(source.Height)
	}

	// Cover fills the box, so the looser side decides, while contain fits inside it, so the tighter side does
	if cover {
		return max(horizontal, vertical)
	}
	return min(horizontal, vertical)
}

// Divide two positive integers, rounding to the nearest integer
func roundDiv(numerator int, denominator int) int {
	return (2*numerator + denominator) / (2 * denominator)
}

// Keep a computed side between one pixel and the source's own side
func clampSide(side int, limit int) int {
	return min(max(side, 1), limit)
}
