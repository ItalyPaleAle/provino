package server

import (
	"net/http"

	httpserver "github.com/italypaleale/go-kit/httpserver"
)

// ResponseError is the structured error body returned by provino API handlers
type ResponseError = httpserver.ApiError

var (
	errInternal = httpserver.NewApiError("internal", http.StatusInternalServerError, "Internal error")

	// The reason a request was refused is logged rather than returned, so a caller cannot probe the issuer by reading the difference
	errUnauthorized = httpserver.NewApiError("unauthorized", http.StatusUnauthorized, "Request is not authorized")
	errForbidden    = httpserver.NewApiError("forbidden", http.StatusForbidden, "The caller does not hold the scope this route requires")

	errInvalidBody      = httpserver.NewApiError("invalid_body", http.StatusBadRequest, "Invalid request body")
	errInvalidMetadata  = httpserver.NewApiError("invalid_metadata", http.StatusBadRequest, "Invalid metadata part")
	errMissingImage     = httpserver.NewApiError("missing_image", http.StatusBadRequest, "Request is missing the image part")
	errInvalidNamespace = httpserver.NewApiError("invalid_namespace", http.StatusBadRequest, "Invalid namespace")
	errInvalidImageID   = httpserver.NewApiError("invalid_image_id", http.StatusBadRequest, "Invalid image ID")
	errInvalidJobID     = httpserver.NewApiError("invalid_job_id", http.StatusBadRequest, "Invalid job ID")
	errInvalidVariants  = httpserver.NewApiError("invalid_variants", http.StatusBadRequest, "Invalid variants")
	errInvalidNotify    = httpserver.NewApiError("invalid_notify", http.StatusBadRequest, "Invalid notification")
	errInvalidFormat    = httpserver.NewApiError("invalid_source_format", http.StatusBadRequest, "Invalid source format")

	errImageExists       = httpserver.NewApiError("image_exists", http.StatusConflict, "An image is already stored with this ID")
	errImageTooLarge     = httpserver.NewApiError("image_too_large", http.StatusRequestEntityTooLarge, "Uploaded image is larger than allowed")
	errUnsupportedFormat = httpserver.NewApiError("unsupported_format", http.StatusUnsupportedMediaType, "Uploaded file is not an image in an accepted format")
	errUnreadableImage   = httpserver.NewApiError("unreadable_image", http.StatusUnsupportedMediaType, "Uploaded image cannot be decoded")
	errTooManyPixels     = httpserver.NewApiError("image_too_many_pixels", http.StatusUnprocessableEntity, "Uploaded image has more pixels than allowed")

	errRateLimited = httpserver.NewApiError("rate_limited", http.StatusTooManyRequests, "Rate limit exceeded")
	errQueueFull   = httpserver.NewApiError("queue_full", http.StatusTooManyRequests, "The image already has the maximum number of jobs waiting")

	// The authorizer refused the upload, which is a decision about this caller rather than about its credentials
	errUploadRefused    = httpserver.NewApiError("upload_refused", http.StatusForbidden, "The upload is not allowed")
	errAuthorizerFailed = httpserver.NewApiError("authorizer_failed", http.StatusBadGateway, "The upload could not be authorized")

	errJobNotFound = httpserver.NewApiError("job_not_found", http.StatusNotFound, "Job not found")
)
