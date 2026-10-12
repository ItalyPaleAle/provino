// Package variant describes the variants provino generates from a source image: their format, size and fit, and the encoder settings they're produced with
//
// It's the vocabulary shared by the HTTP API, the authorizer contract, the manifest and the Go client, and it carries no dependency on libvips, so a caller can import it without cgo
package variant

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Format is one of the image formats variants can be encoded to
type Format string

const (
	FormatWebP Format = "webp"
	FormatAVIF Format = "avif"
	FormatJPEG Format = "jpeg"
	FormatPNG  Format = "png"
)

// Default encoder efforts for the formats that expose the setting
const (
	DefaultAVIFEffort = 4
	DefaultWebPEffort = 5
)

// Fit controls how a source image is mapped onto the requested resolution
type Fit string

const (
	// FitContain scales the image down so it fits entirely within the requested box, preserving the aspect ratio
	// The resulting variant can be smaller than the requested resolution on one of the two axes
	// A source that already fits inside the box is kept at its own size rather than enlarged
	FitContain Fit = "contain"
	// FitCover scales the image down so it covers the requested box, then crops the overflow around the center
	// The resulting variant has exactly the requested resolution, unless the source is smaller than the box on a side
	// Then it's the largest centered crop of the box's shape the source holds, at the source's own resolution
	FitCover Fit = "cover"
)

// ParseFormat parses the name of an image format, accepting the common aliases
func ParseFormat(str string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(str)) {
	case "webp":
		return FormatWebP, nil
	case "avif":
		return FormatAVIF, nil
	case "jpg", "jpeg":
		return FormatJPEG, nil
	case "png":
		return FormatPNG, nil
	default:
		return "", fmt.Errorf("unsupported image format '%s'", str)
	}
}

// Extension returns the file extension used for images in this format, including the leading dot
func (f Format) Extension() string {
	switch f {
	case FormatJPEG:
		return ".jpg"
	default:
		return "." + string(f)
	}
}

// ContentType returns the MIME type of images in this format
func (f Format) ContentType() string {
	switch f {
	case FormatWebP:
		return "image/webp"
	case FormatAVIF:
		return "image/avif"
	case FormatJPEG:
		return "image/jpeg"
	case FormatPNG:
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// SupportsQuality returns true if the format is lossy, so the requested quality changes the encoded output
func (f Format) SupportsQuality() bool {
	return f != FormatPNG
}

// SupportsEffort returns true for formats whose encoders expose a CPU effort setting
func (f Format) SupportsEffort() bool {
	return f == FormatAVIF || f == FormatWebP
}

// DefaultEffort returns the encoder effort used when a request does not provide one
func (f Format) DefaultEffort() int {
	switch f {
	case FormatAVIF:
		return DefaultAVIFEffort
	case FormatWebP:
		return DefaultWebPEffort
	default:
		return 0
	}
}

// ValidateEffort checks the range accepted by the selected encoder
func (f Format) ValidateEffort(effort int) error {
	switch f {
	case FormatAVIF:
		if effort < 0 || effort > 9 {
			return fmt.Errorf("effort must be an integer between 0 and 9 for format '%s'", f)
		}
	case FormatWebP:
		if effort < 0 || effort > 6 {
			return fmt.Errorf("effort must be an integer between 0 and 6 for format '%s'", f)
		}
	default:
		return fmt.Errorf("format '%s' does not support effort", f)
	}

	return nil
}

// OrDefault returns the fit mode, reading an empty one as FitContain the way ParseFit and the encoder do
// Manifests and requests can leave the fit out, and two variants that differ only by that omission are the same variant
func (f Fit) OrDefault() Fit {
	if f == "" {
		return FitContain
	}
	return f
}

// ParseFit parses the name of a fit mode, defaulting to FitContain when empty
func ParseFit(str string) (Fit, error) {
	switch strings.ToLower(strings.TrimSpace(str)) {
	case "":
		return FitContain, nil
	case "contain", "fit", "inside":
		return FitContain, nil
	case "cover", "crop":
		return FitCover, nil
	default:
		return "", fmt.Errorf("unsupported fit mode '%s'", str)
	}
}

// Resolution is a target size for a variant
// A zero Width or Height means that axis is unconstrained, so it's derived from the source image's aspect ratio
type Resolution struct {
	Width  int `json:"width" yaml:"width"`
	Height int `json:"height" yaml:"height"`
}

// ParseResolution parses a resolution in the "<width>x<height>" notation, for example "1200x400"
// Either side can be omitted to leave that axis unconstrained, for example "1200x" or "x400"
func ParseResolution(str string) (res Resolution, err error) {
	str = strings.TrimSpace(str)
	if str == "" {
		return Resolution{}, errors.New("resolution is empty")
	}

	// A bare number is a width, which is the most common way to ask for a variant
	widthStr, heightStr, found := strings.Cut(strings.ToLower(str), "x")
	if !found {
		heightStr = ""
	}

	if widthStr != "" {
		res.Width, err = strconv.Atoi(widthStr)
		if err != nil || res.Width <= 0 {
			return Resolution{}, fmt.Errorf("invalid resolution '%s': width is not a positive integer", str)
		}
	}
	if heightStr != "" {
		res.Height, err = strconv.Atoi(heightStr)
		if err != nil || res.Height <= 0 {
			return Resolution{}, fmt.Errorf("invalid resolution '%s': height is not a positive integer", str)
		}
	}

	if res.Width == 0 && res.Height == 0 {
		return Resolution{}, fmt.Errorf("invalid resolution '%s': at least one of width and height must be set", str)
	}

	return res, nil
}

// String returns the resolution in the "<width>x<height>" notation
func (r Resolution) String() string {
	var width, height string
	if r.Width > 0 {
		width = strconv.Itoa(r.Width)
	}
	if r.Height > 0 {
		height = strconv.Itoa(r.Height)
	}
	return width + "x" + height
}

// Slug returns the resolution in a form that reads well in a file name, with the unconstrained axis spelled out
func (r Resolution) Slug() string {
	width := "auto"
	height := "auto"
	if r.Width > 0 {
		width = strconv.Itoa(r.Width)
	}
	if r.Height > 0 {
		height = strconv.Itoa(r.Height)
	}
	return width + "x" + height
}

// Spec describes one variant to generate
type Spec struct {
	// Format the variant is encoded to: `webp`, `avif`, `jpeg` (or `jpg`), or `png`
	Format Format `json:"format" yaml:"format"`
	// Encoder quality from 1 to 100, required for every format except PNG
	Quality int `json:"quality,omitempty" yaml:"quality,omitempty"`
	// Encoder CPU effort, for AVIF (0-9) and WebP (0-6)
	// When omitted, AVIF uses 4 and WebP uses 5
	Effort *int `json:"effort,omitempty" yaml:"effort,omitempty"`
	// Box the variant is fitted into; either side can be 0 to leave it unconstrained when the fit is `contain`
	Size Resolution `json:"size" yaml:"size"`
	// How the source is mapped onto the box: `contain` (the default) or `cover`
	Fit Fit `json:"fit,omitempty" yaml:"fit,omitempty"`
}

// Normalize validates a spec and returns it in canonical form: the format and fit spelled out and the effort resolved to the value the encoder uses
// maxResolution bounds each side of the box
func (s Spec) Normalize(maxResolution int) (Spec, error) {
	format, err := ParseFormat(string(s.Format))
	if err != nil {
		return Spec{}, err
	}
	fit, err := ParseFit(string(s.Fit))
	if err != nil {
		return Spec{}, err
	}

	// Quality only means something to the lossy encoders, so a PNG that names one is asking for something that can't happen
	switch {
	case format.SupportsQuality() && (s.Quality < 1 || s.Quality > 100):
		return Spec{}, fmt.Errorf("format '%s' requires a quality between 1 and 100", format)
	case !format.SupportsQuality() && s.Quality != 0:
		return Spec{}, fmt.Errorf("format '%s' does not support quality", format)
	}

	// An omitted effort is the encoder's default, which is made explicit so two requests that differ only by that omission describe the same variant
	var effort *int
	switch {
	case format.SupportsEffort() && s.Effort == nil:
		value := format.DefaultEffort()
		effort = &value
	case format.SupportsEffort():
		err = format.ValidateEffort(*s.Effort)
		if err != nil {
			return Spec{}, err
		}
		value := *s.Effort
		effort = &value
	case s.Effort != nil:
		return Spec{}, fmt.Errorf("format '%s' does not support effort", format)
	}

	// Validate the box last, since its rules depend on the fit
	switch {
	case s.Size.Width < 0 || s.Size.Height < 0:
		return Spec{}, fmt.Errorf("size '%s' must not be negative", s.Size)
	case s.Size.Width == 0 && s.Size.Height == 0:
		return Spec{}, errors.New("size must have a width, a height, or both")
	case fit == FitCover && (s.Size.Width == 0 || s.Size.Height == 0):
		return Spec{}, fmt.Errorf("size '%s' must have both a width and a height when fit is 'cover'", s.Size)
	case maxResolution > 0 && (s.Size.Width > maxResolution || s.Size.Height > maxResolution):
		return Spec{}, fmt.Errorf("size '%s' exceeds the maximum of %d pixels per side", s.Size, maxResolution)
	}

	return Spec{
		Format:  format,
		Quality: s.Quality,
		Effort:  effort,
		Size:    s.Size,
		Fit:     fit,
	}, nil
}

// EffortValue returns the effort, or 0 when the spec doesn't set one
func (s Spec) EffortValue() int {
	if s.Effort == nil {
		return 0
	}
	return *s.Effort
}

// Name returns the name a variant is listed under in the manifest
// Cover is in the name because a set can hold the same box with both fits, and their entries have to stay apart
func (s Spec) Name() string {
	name := s.Size.Slug()
	if s.Fit.OrDefault() == FitCover {
		name += "-cover"
	}
	return name + s.Format.Extension()
}

// NormalizeSet validates a list of specs, drops exact duplicates, and returns the rest in canonical form, in the order they were given
//
// Two specs that describe the same box, fit and format but different encoder settings are refused: a variant is stored under its size and format, so the two would overwrite each other
func NormalizeSet(specs []Spec, maxResolution int, maxVariants int) ([]Spec, error) {
	type identity struct {
		format Format
		fit    Fit
		size   Resolution
	}

	out := make([]Spec, 0, len(specs))
	seen := make(map[identity]Spec, len(specs))
	for i, spec := range specs {
		normalized, err := spec.Normalize(maxResolution)
		if err != nil {
			return nil, fmt.Errorf("variant %d: %w", i, err)
		}

		key := identity{format: normalized.Format, fit: normalized.Fit, size: normalized.Size}
		existing, found := seen[key]
		if found {
			if existing.Quality != normalized.Quality || existing.EffortValue() != normalized.EffortValue() {
				return nil, fmt.Errorf("variant %d: conflicts with another variant of the same size, fit and format but different encoder settings", i)
			}
			continue
		}
		seen[key] = normalized
		out = append(out, normalized)
	}

	if maxVariants > 0 && len(out) > maxVariants {
		return nil, fmt.Errorf("the request asks for %d variants, more than the maximum of %d", len(out), maxVariants)
	}

	return out, nil
}
