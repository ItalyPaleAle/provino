package server

import (
	"errors"
	"log/slog"
	"net/http"
	"uuid"

	httpserver "github.com/italypaleale/go-kit/httpserver"

	"github.com/italypaleale/provino/client"
	"github.com/italypaleale/provino/internal/auth"
	"github.com/italypaleale/provino/internal/authorizer"
	"github.com/italypaleale/provino/internal/notify"
)

// RouteIngest is the handler for "POST /v1/public/ingest"
// It stores an image a browser uploads, as the authorizer decides, and queues the job that generates its variants
//
//	@Summary		Upload an image from a browser
//	@Description	Accepts a multipart body with an optional `metadata` part, whose JSON `context` is forwarded to the authorizer, followed by the `image` part. The authorizer decides the namespace, the image ID, the size limit, the accepted formats, the variants and the notification. The image's format is detected from its content. The response is sent once the original is stored and its job is queued.
//	@Tags			public
//	@Accept			multipart/form-data
//	@Produce		json
//	@Security		Bearer
//	@Param			metadata	formData	string	false	"JSON object with an optional `context`, which must come before the image"
//	@Param			image		formData	file	true	"The image"
//	@Success		202			{object}	client.JobAccepted
//	@Failure		400			{object}	client.Error
//	@Failure		401			{object}	client.Error
//	@Failure		403			{object}	client.Error
//	@Failure		409			{object}	client.Error
//	@Failure		413			{object}	client.Error
//	@Failure		415			{object}	client.Error
//	@Failure		422			{object}	client.Error
//	@Failure		429			{object}	client.Error
//	@Failure		502			{object}	client.Error
//	@Router			/v1/public/ingest [post]
func (s *Server) RouteIngest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.appMetrics.RecordAPICall(ctx, "ingest")

	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		errUnauthorized.WriteResponse(w, r)
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		errInvalidBody.Clone(httpserver.WithInnerError(err)).WriteResponse(w, r)
		return
	}

	// The metadata comes first so the authorizer can be asked before a byte of the image is read
	var metadata client.IngestMetadata
	part, _, err := readMetadataPart(mr, notify.MaxContextSize+512, &metadata)
	if err != nil {
		s.respondWithError(w, r, err)
		return
	}
	defer part.Close() //nolint:errcheck
	err = notify.ValidateContext(metadata.Context)
	if err != nil {
		errInvalidMetadata.Clone(httpserver.WithInnerError(err)).WriteResponse(w, r)
		return
	}
	err = checkImagePart(part)
	if err != nil {
		s.respondWithError(w, r, err)
		return
	}

	// The authorizer decides everything about the upload, at the moment of the upload, so a revoked permission or a changed policy applies to the next one
	decision, err := s.authorizer.Authorize(ctx, authorizer.Request{
		Subject:     principal.Subject,
		Context:     metadata.Context,
		FileName:    part.FileName(),
		ContentType: part.Header.Get(httpserver.HeaderContentType),
	})
	if errors.Is(err, authorizer.ErrRefused) {
		// The refusal is the authorizer's decision, which is logged so an operator can tell it apart from a failure without asking the authorizer
		slog.InfoContext(ctx, "The authorizer refused an upload", slog.String("subject", principal.Subject))
		errUploadRefused.WriteResponse(w, r)
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "The authorizer failed to decide about an upload", slog.Any("error", err))
		errAuthorizerFailed.WriteResponse(w, r)
		return
	}

	// A notification the authorizer asks for has to be deliverable, which is the authorizer's mistake rather than the caller's when it isn't
	notification, err := s.resolveNotify(decision.Notify)
	if err != nil {
		slog.ErrorContext(ctx, "The authorizer asked for a notification that can't be delivered", slog.Any("error", err))
		errAuthorizerFailed.WriteResponse(w, r)
		return
	}

	in := upload{
		namespace:      decision.Namespace,
		imageID:        decision.ImageID,
		maxBytes:       decision.MaxBytes,
		allowedFormats: decision.AllowedFormats,
		variants:       decision.Variants,
		notify:         notification,
		subjectHash:    subjectHash(principal.Subject),
		idChosen:       decision.ImageID != uuid.Nil(),
	}
	if !in.idChosen {
		in.imageID = uuid.NewV4()
	}

	// An ID the authorizer chose may already be in use, which is checked before a byte of the image is read
	err = s.claimImageID(ctx, &in)
	if err != nil {
		s.respondWithError(w, r, err)
		return
	}
	defer s.releaseImageID(ctx, in)

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

	slog.InfoContext(ctx, "Accepted a browser upload",
		slog.String("namespace", in.namespace),
		slog.String("imageId", in.imageID.String()),
		slog.String("jobId", accepted.JobID),
		slog.Int64("size", source.Size),
		slog.Int("variants", len(in.variants)),
	)
	respondAccepted(w, r, accepted)
}
