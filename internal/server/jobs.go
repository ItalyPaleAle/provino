package server

import (
	"crypto/subtle"
	"net/http"

	httpserver "github.com/italypaleale/go-kit/httpserver"

	"github.com/italypaleale/provino/client"
	"github.com/italypaleale/provino/imagekeys"
	"github.com/italypaleale/provino/internal/actors"
	"github.com/italypaleale/provino/internal/auth"
)

// RouteClientJobStatus is the handler for "GET /v1/client/images/{namespace}/{imageId}/jobs/{jobId}"
//
//	@Summary		Get the status of a job
//	@Description	Returns the status of any job of an image. A queued job reports its place in the image's queue. The status of a finished job is retained for `actors.completedWorkflowRetention`.
//	@Tags			client
//	@Produce		json
//	@Security		Bearer
//	@Param			namespace	path		string	true	"Namespace of the image"
//	@Param			imageId		path		string	true	"ID of the image"
//	@Param			jobId		path		string	true	"ID of the job"
//	@Success		200			{object}	client.JobStatus
//	@Failure		400			{object}	client.Error
//	@Failure		401			{object}	client.Error
//	@Failure		403			{object}	client.Error
//	@Failure		404			{object}	client.Error
//	@Router			/v1/client/images/{namespace}/{imageId}/jobs/{jobId} [get]
func (s *Server) RouteClientJobStatus(w http.ResponseWriter, r *http.Request) {
	s.appMetrics.RecordAPICall(r.Context(), "client-job-status")
	s.respondJobStatus(w, r, false)
}

// RoutePublicJobStatus is the handler for "GET /v1/public/images/{namespace}/{imageId}/jobs/{jobId}"
//
//	@Summary		Get the status of one's own job
//	@Description	Returns the status of a job the caller started with a browser upload. A job started by anyone else, or through the client API, is reported as not found.
//	@Tags			public
//	@Produce		json
//	@Security		Bearer
//	@Param			namespace	path		string	true	"Namespace of the image"
//	@Param			imageId		path		string	true	"ID of the image"
//	@Param			jobId		path		string	true	"ID of the job"
//	@Success		200			{object}	client.JobStatus
//	@Failure		400			{object}	client.Error
//	@Failure		401			{object}	client.Error
//	@Failure		403			{object}	client.Error
//	@Failure		404			{object}	client.Error
//	@Router			/v1/public/images/{namespace}/{imageId}/jobs/{jobId} [get]
func (s *Server) RoutePublicJobStatus(w http.ResponseWriter, r *http.Request) {
	s.appMetrics.RecordAPICall(r.Context(), "public-job-status")
	s.respondJobStatus(w, r, true)
}

// Look up a job, holding a browser to the jobs it started itself
func (s *Server) respondJobStatus(w http.ResponseWriter, r *http.Request, ownJobsOnly bool) {
	ctx := r.Context()

	// Every identifier is checked before it reaches actor placement or a workflow instance ID
	namespace := r.PathValue("namespace")
	err := imagekeys.ValidateNamespace(namespace)
	if err != nil {
		errInvalidNamespace.Clone(httpserver.WithInnerError(err)).WriteResponse(w, r)
		return
	}

	imageID, err := imagekeys.ValidateID(r.PathValue("imageId"))
	if err != nil {
		errInvalidImageID.Clone(httpserver.WithInnerError(err)).WriteResponse(w, r)
		return
	}

	jobID := r.PathValue("jobId")
	err = actors.ValidateJobID(jobID)
	if err != nil {
		errInvalidJobID.Clone(httpserver.WithInnerError(err)).WriteResponse(w, r)
		return
	}

	status, ownerHash, found, err := s.jobs.Status(ctx, namespace, imageID.String(), jobID)
	if err != nil {
		s.respondWithError(w, r, err)
		return
	}
	if !found {
		errJobNotFound.WriteResponse(w, r)
		return
	}

	// Someone else's job is reported exactly like a job that doesn't exist, so the route says nothing about which IDs are in use
	if ownJobsOnly {
		principal, ok := auth.PrincipalFromContext(ctx)
		if !ok || ownerHash == "" || subtle.ConstantTimeCompare([]byte(ownerHash), []byte(subjectHash(principal.Subject))) != 1 {
			errJobNotFound.WriteResponse(w, r)
			return
		}
	}

	w.Header().Set(httpserver.HeaderContentType, httpserver.ContentTypeJson)
	httpserver.RespondWithJSON(w, r, client.JobStatus{
		JobID:       status.JobID,
		Namespace:   status.Namespace,
		ImageID:     status.ImageID,
		Status:      client.JobState(status.Status),
		Position:    status.Position,
		Total:       status.Total,
		Completed:   status.Completed,
		Error:       status.Error,
		RequestedAt: status.RequestedAt,
		StartedAt:   status.StartedAt,
		CompletedAt: status.CompletedAt,
	})
}
