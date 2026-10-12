package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"uuid"

	httpserver "github.com/italypaleale/go-kit/httpserver"

	"github.com/italypaleale/provino/client"
	"github.com/italypaleale/provino/imagekeys"
	"github.com/italypaleale/provino/internal/actors"
	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/variant"
)

// Largest JSON body or metadata part the client API accepts, which fits the maximum number of variants with room to spare
const maxClientRequestSize = 64 << 10 // 64KB

// RouteUpload is the handler for "POST /v1/client/upload"
// It stores an image a trusted backend uploads and queues the job that generates its variants
//
//	@Summary		Upload an image from a backend
//	@Description	Accepts a multipart body with a `metadata` part, a JSON object naming the namespace and optionally the image ID, the variants and the notification, followed by the `image` part. The image's format is detected from its content. The response is sent once the original is stored and its job is queued.
//	@Tags			client
//	@Accept			multipart/form-data
//	@Produce		json
//	@Security		Bearer
//	@Param			metadata	formData	string	true	"JSON client.UploadMetadata, which must come before the image"
//	@Param			image		formData	file	true	"The image"
//	@Success		202			{object}	client.JobAccepted
//	@Failure		400			{object}	client.Error
//	@Failure		401			{object}	client.Error
//	@Failure		403			{object}	client.Error
//	@Failure		409			{object}	client.Error
//	@Failure		413			{object}	client.Error
//	@Failure		415			{object}	client.Error
//	@Failure		422			{object}	client.Error
//	@Router			/v1/client/upload [post]
func (s *Server) RouteUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.appMetrics.RecordAPICall(ctx, "upload")

	mr, err := r.MultipartReader()
	if err != nil {
		errInvalidBody.Clone(httpserver.WithInnerError(err)).WriteResponse(w, r)
		return
	}

	var metadata client.UploadMetadata
	part, found, err := readMetadataPart(mr, maxClientRequestSize, &metadata)
	if err != nil {
		s.respondWithError(w, r, err)
		return
	}
	defer part.Close() //nolint:errcheck
	if !found {
		errInvalidMetadata.Clone(httpserver.WithInnerError(errors.New("the request must start with a 'metadata' part"))).WriteResponse(w, r)
		return
	}
	err = checkImagePart(part)
	if err != nil {
		s.respondWithError(w, r, err)
		return
	}

	// The whole request is validated before a byte of the image is stored
	in, err := s.validateUploadMetadata(metadata)
	if err != nil {
		s.respondWithError(w, r, err)
		return
	}

	source, err := s.storeUpload(ctx, part, in)
	if err != nil {
		s.respondWithError(w, r, err)
		return
	}

	accepted, err := s.enqueueUpload(ctx, in, source)
	if err != nil {
		s.respondWithError(w, r, err)
		return
	}

	slog.InfoContext(ctx, "Accepted an upload",
		slog.String("namespace", in.namespace),
		slog.String("imageId", in.imageID.String()),
		slog.String("jobId", accepted.JobID),
		slog.Int64("size", source.Size),
		slog.Int("variants", len(in.variants)),
	)
	respondAccepted(w, r, accepted)
}

// Check the metadata of a client upload and turn it into the upload it describes
func (s *Server) validateUploadMetadata(metadata client.UploadMetadata) (upload, error) {
	cfg := config.Get()

	err := imagekeys.ValidateNamespace(metadata.Namespace)
	if err != nil {
		return upload{}, errInvalidNamespace.Clone(httpserver.WithInnerError(err))
	}

	// A new image always gets a v4 ID, which is what keeps the storage folders balanced
	imageID := uuid.NewV4()
	if metadata.ImageID != "" {
		imageID, err = imagekeys.ValidateID(metadata.ImageID)
		if err != nil {
			return upload{}, errInvalidImageID.Clone(httpserver.WithInnerError(err))
		}
	}

	variants, err := variant.NormalizeSet(metadata.Variants, cfg.Images.MaxResolution, cfg.Images.MaxVariants)
	if err != nil {
		return upload{}, errInvalidVariants.Clone(httpserver.WithInnerError(err))
	}

	notification, err := s.resolveNotify(metadata.Notify)
	if err != nil {
		return upload{}, err
	}

	// Jobs started by a backend carry no subject, so no browser can read them through the public route
	return upload{
		namespace:      metadata.Namespace,
		imageID:        imageID,
		allowedFormats: config.SourceFormats,
		variants:       variants,
		notify:         notification,
	}, nil
}

// RouteOptimize is the handler for "POST /v1/client/optimize"
// It queues a job that generates variants for an image already in storage
//
//	@Summary		Generate variants for an existing image
//	@Description	Queues a job for an image that is already stored. Send every variant the image should have, including the ones it already has: stored files that still match are reused, boxes the original can't fill are skipped, and the rest are encoded. The request is validated and queued without touching storage, so an image that doesn't exist makes the job fail with `image_not_found` rather than this request.
//	@Tags			client
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			request	body		client.OptimizeRequest	true	"The image and its variants"
//	@Success		202		{object}	client.JobAccepted
//	@Failure		400		{object}	client.Error
//	@Failure		401		{object}	client.Error
//	@Failure		403		{object}	client.Error
//	@Failure		429		{object}	client.Error
//	@Router			/v1/client/optimize [post]
func (s *Server) RouteOptimize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.appMetrics.RecordAPICall(ctx, "optimize")
	cfg := config.Get()

	var req client.OptimizeRequest
	data, err := io.ReadAll(io.LimitReader(r.Body, maxClientRequestSize+1))
	if err != nil || len(data) > maxClientRequestSize {
		errInvalidBody.Clone(httpserver.WithInnerError(fmt.Errorf("the body must be a JSON object of at most %d bytes", maxClientRequestSize))).WriteResponse(w, r)
		return
	}
	err = json.Unmarshal(data, &req)
	if err != nil {
		errInvalidBody.Clone(httpserver.WithInnerError(err)).WriteResponse(w, r)
		return
	}

	err = imagekeys.ValidateNamespace(req.Namespace)
	if err != nil {
		errInvalidNamespace.Clone(httpserver.WithInnerError(err)).WriteResponse(w, r)
		return
	}

	imageID, err := imagekeys.ValidateID(req.ImageID)
	if err != nil {
		errInvalidImageID.Clone(httpserver.WithInnerError(err)).WriteResponse(w, r)
		return
	}

	if len(req.Variants) == 0 {
		errInvalidVariants.Clone(httpserver.WithInnerError(errors.New("the request names no variants"))).WriteResponse(w, r)
		return
	}
	variants, err := variant.NormalizeSet(req.Variants, cfg.Images.MaxResolution, cfg.Images.MaxVariants)
	if err != nil {
		errInvalidVariants.Clone(httpserver.WithInnerError(err)).WriteResponse(w, r)
		return
	}

	sourceFormat := ""
	if req.SourceFormat != "" {
		sourceFormat = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(req.SourceFormat), "."))
		if sourceFormat == "jpeg" {
			sourceFormat = "jpg"
		}
		if !slices.Contains(config.SourceFormats, sourceFormat) {
			errInvalidFormat.Clone(httpserver.WithInnerError(fmt.Errorf("'%s' is not one of %s", req.SourceFormat, strings.Join(config.SourceFormats, ", ")))).WriteResponse(w, r)
			return
		}
	}

	notification, err := s.resolveNotify(req.Notify)
	if err != nil {
		s.respondWithError(w, r, err)
		return
	}

	res, err := s.jobs.Enqueue(ctx, actors.JobRequest{
		Namespace:    req.Namespace,
		ImageID:      imageID.String(),
		SourceFormat: sourceFormat,
		Variants:     jobVariants(variants),
		Notify:       notification,
		RequestedAt:  time.Now(),
	})
	if errors.Is(err, actors.ErrQueueFull) {
		errQueueFull.WriteResponse(w, r)
		return
	} else if err != nil {
		s.respondWithError(w, r, err)
		return
	}

	respondAccepted(w, r, client.JobAccepted{
		Namespace:   req.Namespace,
		ImageID:     imageID.String(),
		JobID:       res.JobID,
		Status:      client.JobQueued,
		Position:    res.Position,
		ManifestKey: s.store.ManifestKey(req.Namespace, imageID),
	})
}
