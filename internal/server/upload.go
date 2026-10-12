package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
	"uuid"

	httpserver "github.com/italypaleale/go-kit/httpserver"

	"github.com/italypaleale/provino/client"
	"github.com/italypaleale/provino/internal/actors"
	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/internal/notify"
	"github.com/italypaleale/provino/internal/sniff"
	"github.com/italypaleale/provino/internal/store"
	"github.com/italypaleale/provino/manifest"
	"github.com/italypaleale/provino/variant"
	"github.com/italypaleale/provino/webhook"
)

// Names of the multipart parts an upload carries, in the order they must come
const (
	metadataPartName = "metadata"
	imagePartName    = "image"
)

// upload describes an upload that is allowed, before its image is read
type upload struct {
	namespace      string
	imageID        uuid.UUID
	maxBytes       int64
	allowedFormats []string
	variants       []variant.Spec
	notify         *webhook.Notify
	subjectHash    string
}

// readMetadataPart reads the metadata part when it's the next part, returning the part after it
// Every part is read in order because the image is streamed straight to the store, so the metadata has to arrive first for the destination to be known before the first byte of the image is written
func readMetadataPart(mr *multipart.Reader, maxSize int64, into any) (*multipart.Part, bool, error) {
	part, err := mr.NextPart()
	if errors.Is(err, io.EOF) {
		return nil, false, errMissingImage
	} else if err != nil {
		return nil, false, errInvalidBody.Clone(httpserver.WithInnerError(err))
	}
	if part.FormName() != metadataPartName {
		return part, false, nil
	}

	// Reading one byte past the limit is what tells a part that's exactly at the limit apart from one that's over it
	data, err := io.ReadAll(io.LimitReader(part, maxSize+1))
	_ = part.Close()
	if err != nil {
		return nil, true, errInvalidMetadata.Clone(httpserver.WithInnerError(err))
	}
	if int64(len(data)) > maxSize {
		return nil, true, errInvalidMetadata.Clone(httpserver.WithInnerError(fmt.Errorf("the metadata part is larger than %d bytes", maxSize)))
	}
	err = json.Unmarshal(data, into)
	if err != nil {
		return nil, true, errInvalidMetadata.Clone(httpserver.WithInnerError(err))
	}

	part, err = mr.NextPart()
	if errors.Is(err, io.EOF) {
		return nil, true, errMissingImage
	} else if err != nil {
		return nil, true, errInvalidBody.Clone(httpserver.WithInnerError(err))
	}
	return part, true, nil
}

// checkImagePart confirms a part is the image
func checkImagePart(part *multipart.Part) error {
	if part.FormName() != imagePartName || part.FileName() == "" {
		return errMissingImage.Clone(httpserver.WithInnerError(fmt.Errorf("expected a file in the '%s' part, but got the part '%s'", imagePartName, part.FormName())))
	}
	return nil
}

// storeUpload stores an uploaded image and records it in its manifest, returning what the job needs to know about it
//
// The format is decided from the file's leading bytes, so neither its name nor the content type the client declared can make this service store something it doesn't accept
// The image is written to the store as it streams by, and a copy goes into the local source cache, so the job that follows doesn't have to download it again
func (s *Server) storeUpload(ctx context.Context, part *multipart.Part, in upload) (actors.JobSource, error) {
	cfg := config.Get()

	// Sniff the format from what's buffered, without consuming it, so every byte still reaches the store
	buffered := bufio.NewReaderSize(part, sniff.HeaderSize)
	header, err := buffered.Peek(sniff.HeaderSize)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return actors.JobSource{}, errInvalidBody.Clone(httpserver.WithInnerError(err))
	}
	format, contentType, ok := sniff.Detect(header)
	if !ok || !slices.Contains(in.allowedFormats, format) {
		return actors.JobSource{}, errUnsupportedFormat.Clone(httpserver.WithMetadata(map[string]string{"allowed": fmt.Sprint(in.allowedFormats)}))
	}

	maxSize := cfg.Images.MaxUploadSize
	if in.maxBytes > 0 {
		maxSize = min(maxSize, in.maxBytes)
	}

	// The cache copy is a temporary file until the upload is accepted, and it's removed on every path that doesn't adopt it
	tmp, err := s.sourceCache.CreateTemp()
	if err != nil {
		return actors.JobSource{}, fmt.Errorf("failed to create the cached copy of the upload: %w", err)
	}
	adopted := false
	defer func() {
		if !adopted {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	_, size, err := s.store.SaveOriginal(ctx, in.namespace, in.imageID, format, io.TeeReader(buffered, tmp), maxSize, contentType)
	switch {
	case errors.Is(err, store.ErrAlreadyExists):
		return actors.JobSource{}, errImageExists
	case errors.Is(err, store.ErrTooLarge):
		return actors.JobSource{}, errImageTooLarge.Clone(httpserver.WithMetadata(map[string]string{"maximum": strconv.FormatInt(maxSize, 10)}))
	case err != nil:
		return actors.JobSource{}, fmt.Errorf("failed to store the uploaded image: %w", err)
	}

	// From here on the original is stored, so every rejection removes it
	source, err := s.measureUpload(ctx, tmp)
	if err != nil {
		s.removeUpload(ctx, in, format)
		return actors.JobSource{}, err
	}
	source.Format = format
	source.Name = filepath.Base(part.FileName())
	source.Size = size
	source.StoredAt = time.Now().UTC()

	// The cache only saves a download, so failing to keep the copy doesn't fail the upload
	err = s.sourceCache.Adopt(tmp.Name(), in.namespace, in.imageID)
	if err != nil {
		slog.WarnContext(ctx, "Failed to keep the uploaded image in the source cache", slog.Any("error", err))
	} else {
		adopted = true
	}

	// The manifest records the image as soon as it's stored, so it's never stored without being recorded, whatever happens to its jobs
	err = s.store.WriteManifest(ctx, in.namespace, in.imageID, manifest.Manifest{
		Version:      manifest.Version,
		Namespace:    in.namespace,
		ImageID:      in.imageID.String(),
		Status:       manifest.StatusStored,
		SourceKey:    s.store.OriginalKey(in.namespace, in.imageID, format),
		SourceName:   source.Name,
		SourceFormat: format,
		SourceSize:   size,
		SourceWidth:  source.Width,
		SourceHeight: source.Height,
		StoredAt:     source.StoredAt,
		Variants:     []manifest.Variant{},
	})
	if err != nil {
		s.removeUpload(ctx, in, format)
		return actors.JobSource{}, fmt.Errorf("failed to write the manifest: %w", err)
	}

	return source, nil
}

// Read the uploaded image's size from the copy written while it streamed, and enforce the pixel limit
func (s *Server) measureUpload(ctx context.Context, tmp *os.File) (actors.JobSource, error) {
	err := tmp.Close()
	if err != nil {
		return actors.JobSource{}, fmt.Errorf("failed to close the uploaded copy: %w", err)
	}
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		return actors.JobSource{}, fmt.Errorf("failed to read the uploaded copy: %w", err)
	}

	size, err := s.measurer.SourceSize(ctx, data)
	if err != nil {
		return actors.JobSource{}, errUnreadableImage.Clone(httpserver.WithInnerError(err))
	}

	// The limit is on pixels rather than sides, because what decoding costs is the area, and a long panorama is as reasonable as a square
	maxPixels := config.Get().Images.MaxSourcePixels
	if int64(size.Width)*int64(size.Height) > maxPixels {
		return actors.JobSource{}, errTooManyPixels.Clone(httpserver.WithMetadata(map[string]string{
			"width":   strconv.Itoa(size.Width),
			"height":  strconv.Itoa(size.Height),
			"maximum": strconv.FormatInt(maxPixels, 10),
		}))
	}

	return actors.JobSource{Width: size.Width, Height: size.Height}, nil
}

// Queue the job of an upload, removing everything the upload wrote when it can't be queued
func (s *Server) enqueueUpload(ctx context.Context, in upload, source actors.JobSource) (client.JobAccepted, error) {
	res, err := s.jobs.Enqueue(ctx, actors.JobRequest{
		Namespace:   in.namespace,
		ImageID:     in.imageID.String(),
		Source:      &source,
		Variants:    jobVariants(in.variants),
		Notify:      in.notify,
		SubjectHash: in.subjectHash,
		RequestedAt: time.Now(),
	})
	if err != nil {
		s.removeUpload(ctx, in, source.Format)
		if errors.Is(err, actors.ErrQueueFull) {
			return client.JobAccepted{}, errQueueFull
		}
		return client.JobAccepted{}, fmt.Errorf("failed to queue the job: %w", err)
	}

	return client.JobAccepted{
		Namespace:   in.namespace,
		ImageID:     in.imageID.String(),
		JobID:       res.JobID,
		Status:      client.JobQueued,
		Position:    res.Position,
		SourceKey:   s.store.OriginalKey(in.namespace, in.imageID, source.Format),
		ManifestKey: s.store.ManifestKey(in.namespace, in.imageID),
	}, nil
}

// Remove what an upload wrote: its original, its manifest, and its cached copy
// The request's context is often already canceled at this point, so the deletes get one of their own
func (s *Server) removeUpload(ctx context.Context, in upload, format string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	err := errors.Join(
		s.store.RemoveOriginal(ctx, in.namespace, in.imageID, format),
		s.store.RemoveManifest(ctx, in.namespace, in.imageID),
		s.sourceCache.Remove(in.namespace, in.imageID),
	)
	if err != nil {
		slog.WarnContext(ctx, "Failed to remove what a rejected upload had written",
			slog.String("namespace", in.namespace),
			slog.String("imageId", in.imageID.String()),
			slog.Any("error", err),
		)
	}
}

// Resolve a request's notification against the configured endpoints
func (s *Server) resolveNotify(n *webhook.Notify) (*webhook.Notify, error) {
	if n == nil {
		return nil, nil //nolint:nilnil // A request without a notification asks for none
	}
	if s.notifier == nil {
		return nil, errInvalidNotify.Clone(httpserver.WithInnerError(errors.New("no webhook endpoints are configured")))
	}

	endpoint, err := s.notifier.Resolve(n.Endpoint)
	if err != nil {
		return nil, errInvalidNotify.Clone(httpserver.WithInnerError(err))
	}
	err = notify.ValidateContext(n.Context)
	if err != nil {
		return nil, errInvalidNotify.Clone(httpserver.WithInnerError(err))
	}

	return &webhook.Notify{Endpoint: endpoint, Context: n.Context}, nil
}

// Turn validated variant specs into the form a job carries
func jobVariants(specs []variant.Spec) []actors.VariantSpec {
	out := make([]actors.VariantSpec, len(specs))
	for i, spec := range specs {
		out[i] = actors.VariantSpec{
			Index:      i,
			Name:       spec.Name(),
			Format:     spec.Format,
			Quality:    spec.Quality,
			Effort:     spec.EffortValue(),
			Resolution: spec.Size,
			Fit:        spec.Fit,
		}
	}
	return out
}

// Hash a subject, which is what a job stores to know who started it without storing who that is
func subjectHash(subject string) string {
	digest := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(digest[:])
}

// Write a job that was accepted
func respondAccepted(w http.ResponseWriter, r *http.Request, accepted client.JobAccepted) {
	w.Header().Set(httpserver.HeaderContentType, httpserver.ContentTypeJson)
	w.WriteHeader(http.StatusAccepted)
	httpserver.RespondWithJSON(w, r, accepted)
}
