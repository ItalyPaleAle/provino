package imaging

import (
	"github.com/italypaleale/provino/imagesize"
)

// FullResolution reports whether a source can't fill a box, so the variant for that box keeps the source's own pixels instead of scaling them up
// When it can't, the returned size is that variant's: the whole source with contain, or the largest centered crop of the box's shape with cover
// The geometry lives in the public imagesize package, because an application can plan the same boxes before it asks for them
func FullResolution(source Resolution, box Resolution, fit Fit) (Resolution, bool) {
	size, full := imagesize.FullResolution(imagesize.Size(source), imagesize.Size(box), fit == FitCover)
	return Resolution(size), full
}

// ScaleFactor returns how much a variant for a box would scale a source if it were allowed to enlarge it
// Below 1 the variant shrinks the source, and among the boxes a source can't fill, the one with the smallest factor is the tightest
func ScaleFactor(source Resolution, box Resolution, fit Fit) float64 {
	return imagesize.ScaleFactor(imagesize.Size(source), imagesize.Size(box), fit == FitCover)
}
