package imaging

import (
	"github.com/italypaleale/provino/variant"
)

// The variant vocabulary lives in the public variant package, which has no cgo dependency, and the encoder uses it as its own
type (
	Format     = variant.Format
	Fit        = variant.Fit
	Resolution = variant.Resolution
)

const (
	FormatWebP = variant.FormatWebP
	FormatAVIF = variant.FormatAVIF
	FormatJPEG = variant.FormatJPEG
	FormatPNG  = variant.FormatPNG

	FitContain = variant.FitContain
	FitCover   = variant.FitCover

	DefaultAVIFEffort = variant.DefaultAVIFEffort
	DefaultWebPEffort = variant.DefaultWebPEffort
)
