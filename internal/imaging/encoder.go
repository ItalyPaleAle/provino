package imaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/davidbyttow/govips/v2/vips"
)

var (
	// ErrInvalidRequest marks a request the encoder can't ever satisfy, such as one asking for an unsupported format
	// Callers use it to tell apart the failures that are worth retrying from the ones that aren't
	ErrInvalidRequest = errors.New("invalid variant request")

	// ErrSourceUnreadable marks a source image libvips can't decode, which doesn't change if the same request is attempted again
	ErrSourceUnreadable = errors.New("source image cannot be read")

	errEmptyResolution = fmt.Errorf("%w: resolution is empty", ErrInvalidRequest)
)

// Bound used for the axis of a resolution that was left unconstrained
// libvips picks the scale factor from whichever axis binds first, so a very large bound on one axis makes the other one decide the size
const unconstrainedDimension = 1 << 20 // 1MB

// Number of threads libvips uses within a single operation
//
// Variants are encoded in parallel, one goroutine each, so parallelism belongs between operations rather than inside them: giving each operation its own thread pool would multiply the two and oversubscribe the CPU
// The number of variants encoded at the same time is what the operator tunes instead, with "images.maxConcurrentEncodes"
const vipsConcurrency = 1

var startupOnce sync.Once

// Options contains the process-wide settings for libvips
type Options struct {
	// Maximum amount of memory, in bytes, for the libvips operation cache
	CacheMaxMemory int
	// Logger that receives the libvips diagnostic messages
	Logger *slog.Logger
}

// Startup initializes libvips
// It must be called once before any variant is generated, and libvips cannot be restarted after Shutdown, so repeated calls are ignored
func Startup(ctx context.Context, opts Options) error {
	var err error
	startupOnce.Do(func() {
		// Route the libvips diagnostics through slog, so they land in the same place as the rest of the application's logs
		logger := opts.Logger
		if logger == nil {
			logger = slog.Default()
		}
		vips.LoggingSettings(func(domain string, level vips.LogLevel, msg string) {
			logger.Log(ctx, vipsLogLevel(level), msg, slog.String("domain", domain))
		}, vips.LogLevelWarning)

		err = vips.Startup(&vips.Config{
			ConcurrencyLevel: vipsConcurrency,
			MaxCacheMem:      opts.CacheMaxMemory,
		})
	})
	if err != nil {
		return fmt.Errorf("failed to initialize libvips: %w", err)
	}

	return nil
}

// Shutdown releases the resources held by libvips
// It cannot be started again afterwards, so this is only meant to be called while the process is terminating
func Shutdown() {
	vips.Shutdown()
}

// Encoder generates variants with libvips
type Encoder struct {
}

// NewEncoder returns a new Encoder
// Startup must have been called before the encoder generates anything
func NewEncoder() *Encoder {
	return &Encoder{}
}

// Request describes a single variant to generate
type Request struct {
	// Source image to generate the variant from
	// It's passed as a buffer rather than as a path because the source is stored in an object store, which may not be a local disk
	Source []byte
	// Format the variant is encoded to
	Format Format
	// Quality for lossy formats, between 1 and 100; ignored by the formats that are lossless
	Quality int
	// CPU effort for AVIF and WebP encoding
	Effort int
	// Target resolution
	Resolution Resolution
	// How the source image is mapped onto the target resolution
	Fit Fit
}

// Result describes a variant that was generated
type Result struct {
	// Encoded variant, which the caller writes to the store
	Data []byte
	// Actual width of the variant, which can be smaller than the requested one when fitting inside the requested box, or when the source is smaller than the box
	Width int
	// Actual height of the variant
	Height int
	// Size of the encoded variant, in bytes
	Size int64
}

// Generate creates a single variant and returns it encoded
// The variant is never larger than the source: a box the source can't fill gets the source's own pixels instead
func (e *Encoder) Generate(ctx context.Context, req Request) (Result, error) {
	err := req.validate()
	if err != nil {
		return Result{}, err
	}

	// A caller that already gave up should not start a CPU-intensive encoding operation
	err = ctx.Err()
	if err != nil {
		//nolint:wrapcheck
		return Result{}, err
	}

	// A cover box the source can't fill gets the largest crop of the box's shape the source holds, which libvips doesn't compute on its own
	// Shrinking only, libvips would keep the source's size on the short side and crop the other to the box, which changes the shape
	if req.Fit == FitCover {
		source, sizeErr := e.SourceSize(ctx, req.Source)
		if sizeErr != nil {
			return Result{}, sizeErr
		}
		full, isFull := FullResolution(source, req.Resolution, req.Fit)
		if isFull {
			req.Resolution = full
		}
	}

	// libvips shrinks the image while decoding it, so this never materializes the full-size image when the source is much larger than the variant
	// Shrinking only is what keeps a variant from ever being larger than its source: a contain box the source fits in keeps the source's own size
	width, height, crop := req.thumbnailParams()
	img, err := vips.NewThumbnailWithSizeFromBuffer(req.Source, width, height, crop, vips.SizeDown)
	if err != nil {
		// The source image never changes once it's uploaded, so a load failure is permanent rather than something to retry
		return Result{}, fmt.Errorf("failed to generate variant: %w", errors.Join(ErrSourceUnreadable, err))
	}
	defer img.Close()

	buf, err := e.export(img, req)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Data:   buf,
		Width:  img.Width(),
		Height: img.Height(),
		Size:   int64(len(buf)),
	}, nil
}

// SourceSize returns the size of a source image the way its variants see it, after its EXIF orientation is applied
func (e *Encoder) SourceSize(ctx context.Context, source []byte) (Resolution, error) {
	if len(source) == 0 {
		return Resolution{}, fmt.Errorf("%w: source image is empty", ErrInvalidRequest)
	}
	err := ctx.Err()
	if err != nil {
		//nolint:wrapcheck
		return Resolution{}, err
	}

	// Loading reads only the header, because libvips decodes the pixels when something first needs them
	// Failing on warnings is turned off to match the thumbnail operation, which tolerates the same minor damage
	params := vips.NewImportParams()
	params.FailOnError.Set(false)
	img, err := vips.LoadImageFromBuffer(source, params)
	if err == nil {
		defer img.Close()
		return orientedSize(img.Width(), img.Height(), img.Orientation()), nil
	}

	// govips recognizes formats by their leading bytes and misses some files libvips itself reads, such as HEIF files with less common brands
	// Measuring through the thumbnail operation recognizes exactly what the encoder does, at the cost of decoding the image
	probe, probeErr := vips.NewThumbnailWithSizeFromBuffer(source, unconstrainedDimension, unconstrainedDimension, vips.InterestingNone, vips.SizeDown)
	if probeErr != nil {
		return Resolution{}, fmt.Errorf("failed to read the size of the source image: %w", errors.Join(ErrSourceUnreadable, err, probeErr))
	}
	defer probe.Close()

	// The thumbnail operation already applied the orientation
	return Resolution{Width: probe.Width(), Height: probe.Height()}, nil
}

// Orientations 5 to 8 rotate the image a quarter turn, some of them mirrored too, and libvips' thumbnails apply them, so the variants see the two sides swapped
func orientedSize(width int, height int, orientation int) Resolution {
	if orientation >= 5 && orientation <= 8 {
		return Resolution{Width: height, Height: width}
	}
	return Resolution{Width: width, Height: height}
}

// Encode the resized image in the requested format
func (e *Encoder) export(img *vips.ImageRef, req Request) ([]byte, error) {
	var (
		buf []byte
		err error
	)
	switch req.Format {
	case FormatWebP:
		params := vips.NewWebpExportParams()
		params.Quality = req.Quality
		params.ReductionEffort = req.Effort
		params.StripMetadata = true
		buf, _, err = img.ExportWebp(params)
	case FormatAVIF:
		params := vips.NewAvifExportParams()
		params.Quality = req.Quality
		params.Effort = req.Effort
		params.StripMetadata = true
		buf, _, err = img.ExportAvif(params)
	case FormatJPEG:
		params := vips.NewJpegExportParams()
		params.Quality = req.Quality
		params.StripMetadata = true
		buf, _, err = img.ExportJpeg(params)
	case FormatPNG:
		// PNG is lossless, so the requested quality doesn't apply here
		params := vips.NewPngExportParams()
		params.StripMetadata = true
		buf, _, err = img.ExportPng(params)
	default:
		return nil, fmt.Errorf("%w: unsupported image format '%s'", ErrInvalidRequest, req.Format)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to encode variant as %s: %w", req.Format, err)
	}

	return buf, nil
}

// Validate the request, catching the combinations libvips would either reject or silently reinterpret
func (r Request) validate() error {
	switch {
	case len(r.Source) == 0:
		return fmt.Errorf("%w: source image is empty", ErrInvalidRequest)
	case r.Resolution.Width <= 0 && r.Resolution.Height <= 0:
		return errEmptyResolution
	case r.Fit == FitCover && (r.Resolution.Width <= 0 || r.Resolution.Height <= 0):
		return fmt.Errorf("%w: fit mode 'cover' requires both a width and a height", ErrInvalidRequest)
	case r.Format.SupportsQuality() && (r.Quality < 1 || r.Quality > 100):
		// Lossy formats carry their own quality in the request, and there's no default to fall back on
		return fmt.Errorf("%w: quality must be between 1 and 100 for format '%s'", ErrInvalidRequest, r.Format)
	case r.Format.SupportsEffort():
		err := r.Format.ValidateEffort(r.Effort)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
		return nil
	case r.Effort != 0:
		return fmt.Errorf("%w: format '%s' does not support effort", ErrInvalidRequest, r.Format)
	default:
		return nil
	}
}

// Translate the request's resolution and fit mode into the arguments of the libvips thumbnail operation
func (r Request) thumbnailParams() (width int, height int, crop vips.Interesting) {
	width = r.Resolution.Width
	height = r.Resolution.Height
	if width <= 0 {
		width = unconstrainedDimension
	}
	if height <= 0 {
		height = unconstrainedDimension
	}

	// "cover" crops the overflow around the center, while "contain" only ever scales
	crop = vips.InterestingNone
	if r.Fit == FitCover {
		crop = vips.InterestingCentre
	}

	return width, height, crop
}

// Map a libvips log level to the equivalent slog one
func vipsLogLevel(level vips.LogLevel) slog.Level {
	switch level {
	case vips.LogLevelError, vips.LogLevelCritical:
		return slog.LevelError
	case vips.LogLevelWarning:
		return slog.LevelWarn
	case vips.LogLevelMessage, vips.LogLevelInfo:
		return slog.LevelInfo
	default:
		return slog.LevelDebug
	}
}
