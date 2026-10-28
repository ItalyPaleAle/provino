package actors

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/italypaleale/francis/actor"
	francisworkflow "github.com/italypaleale/francis/builtin/workflow"

	"github.com/italypaleale/provino/imagekeys"
)

var (
	// ErrQueueFull is returned when an image already has as many jobs waiting as it may
	ErrQueueFull = errors.New("the image already has the maximum number of jobs waiting")

	// ErrUploadInProgress is returned for a job of an image that an upload is still storing, which can be queued again once the upload is done
	ErrUploadInProgress = errors.New("the image is still being uploaded")
)

// Jobs is what the API handlers use to queue jobs and read their status
type Jobs interface {
	// Enqueue adds a job to its image's queue and returns its ID, or ErrQueueFull or ErrUploadInProgress
	Enqueue(ctx context.Context, req JobRequest) (EnqueueResult, error)
	// Status returns a job's status, and the hash of the subject that started it when that's still known
	// The boolean is false when the image has no such job, or when its status has expired
	Status(ctx context.Context, namespace string, imageID string, jobID string) (JobStatus, string, bool, error)
	// ClaimUpload holds an image's ID for one upload that names it, identified by token, for at most ttl
	// It returns false when another upload holds the ID, or when a job of the image is queued or running
	ClaimUpload(ctx context.Context, namespace string, imageID string, token string, ttl time.Duration) (bool, error)
	// ReleaseUpload gives up the claim the upload identified by token holds, which does nothing when it holds none
	ReleaseUpload(ctx context.Context, namespace string, imageID string, token string) error
}

// EnqueueResult is a job that was accepted
type EnqueueResult struct {
	JobID string
	// 0 when the job started right away, its 1-based place in the queue otherwise
	Position int
}

// jobService starts and inspects job workflows
type jobService struct {
	workflow *francisworkflow.WorkflowService
	runtime  *Runtime
}

// Durably start the workflow of a job
// Starting the same job twice is harmless, because the instance ID is the job's own
func (s *jobService) start(ctx context.Context, req JobRequest) error {
	err := imagekeys.ValidateNamespace(req.Namespace)
	if err != nil {
		return fmt.Errorf("invalid namespace: %w", err)
	}

	_, err = imagekeys.ValidateID(req.ImageID)
	if err != nil {
		return fmt.Errorf("invalid image ID: %w", err)
	}

	err = ValidateJobID(req.JobID)
	if err != nil {
		return err
	}

	// The position is the fan-out identity, so normalize it before the input becomes durable
	req.Variants = append([]VariantSpec(nil), req.Variants...)
	for i := range req.Variants {
		req.Variants[i].Index = i
	}
	if req.RequestedAt.IsZero() {
		req.RequestedAt = time.Now()
	}

	_, created, err := s.workflow.Start(ctx, req,
		francisworkflow.WithInstanceID(jobInstanceID(req.Namespace, req.ImageID, req.JobID)),
	)
	if err != nil {
		return fmt.Errorf("failed to start the workflow of job '%s': %w", req.JobID, err)
	}
	if created && s.runtime.Metrics != nil {
		s.runtime.Metrics.RecordJobStarted(ctx, len(req.Variants))
	}

	return nil
}

// Read the journal of a job's workflow
func (s *jobService) instanceStatus(ctx context.Context, namespace string, imageID string, jobID string) (francisworkflow.InstanceStatus, bool, error) {
	status, err := s.workflow.GetStatus(ctx, jobInstanceID(namespace, imageID, jobID))
	if errors.Is(err, francisworkflow.ErrInstanceNotFound) {
		return francisworkflow.InstanceStatus{}, false, nil
	} else if err != nil {
		return francisworkflow.InstanceStatus{}, false, fmt.Errorf("failed to read the workflow of job '%s': %w", jobID, err)
	}
	return status, true, nil
}

// Report whether a workflow has ended, one way or another
func isTerminal(status francisworkflow.InstanceStatus) bool {
	switch status.Status {
	case francisworkflow.StatusCompleted, francisworkflow.StatusFailed, francisworkflow.StatusCancelled:
		return true
	default:
		return false
	}
}

// ValidateJobID checks a job ID is a canonical UUIDv7, which is the only kind the image actor mints
func ValidateJobID(id string) error {
	parsed, err := imagekeys.ValidateID(id)
	if err != nil {
		return fmt.Errorf("invalid job ID: %w", err)
	}
	if parsed[6]>>4 != 7 {
		return fmt.Errorf("invalid job ID: '%s' is not a UUIDv7", id)
	}
	return nil
}

// hostJobs implements Jobs over the image actors and the job workflows
type hostJobs struct {
	service *actor.Service
	jobs    *jobService
}

func (h *hostJobs) Enqueue(ctx context.Context, req JobRequest) (EnqueueResult, error) {
	res, err := h.service.Invoke(ctx, imageActorType, imageActorID(req.Namespace, req.ImageID), methodEnqueue, req)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("failed to queue the job: %w", err)
	}

	var result enqueueResult
	err = res.Decode(&result)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("failed to decode the queued job: %w", err)
	}
	if result.QueueFull {
		return EnqueueResult{}, ErrQueueFull
	}
	if result.UploadInProgress {
		return EnqueueResult{}, ErrUploadInProgress
	}

	return EnqueueResult{JobID: result.JobID, Position: result.Position}, nil
}

func (h *hostJobs) ClaimUpload(ctx context.Context, namespace string, imageID string, token string, ttl time.Duration) (bool, error) {
	res, err := h.service.Invoke(ctx, imageActorType, imageActorID(namespace, imageID), methodClaim, claimRequest{Token: token, TTL: ttl})
	if err != nil {
		return false, fmt.Errorf("failed to claim the image ID: %w", err)
	}

	var result claimResult
	err = res.Decode(&result)
	if err != nil {
		return false, fmt.Errorf("failed to decode the claim: %w", err)
	}
	return result.Claimed, nil
}

func (h *hostJobs) ReleaseUpload(ctx context.Context, namespace string, imageID string, token string) error {
	_, err := h.service.Invoke(ctx, imageActorType, imageActorID(namespace, imageID), methodRelease, claimRequest{Token: token})
	if err != nil {
		return fmt.Errorf("failed to release the image ID: %w", err)
	}
	return nil
}

func (h *hostJobs) Status(ctx context.Context, namespace string, imageID string, jobID string) (JobStatus, string, bool, error) {
	// The image's actor knows the jobs that are waiting, which have no workflow yet, and who started the recent ones
	res, err := h.service.Peek(ctx, imageActorType, imageActorID(namespace, imageID), methodJob, finishedRequest{JobID: jobID})
	if err != nil {
		return JobStatus{}, "", false, fmt.Errorf("failed to look up the job: %w", err)
	}
	var lookup jobLookup
	err = res.Decode(&lookup)
	if err != nil {
		return JobStatus{}, "", false, fmt.Errorf("failed to decode the job lookup: %w", err)
	}

	status := JobStatus{
		JobID:     jobID,
		Namespace: namespace,
		ImageID:   imageID,
	}
	if lookup.State == "queued" {
		status.Status = StatusQueued
		status.Position = lookup.Position
		return status, lookup.SubjectHash, true, nil
	}

	instance, found, err := h.jobs.instanceStatus(ctx, namespace, imageID, jobID)
	if err != nil {
		return JobStatus{}, "", false, err
	}
	switch {
	case found:
		applyInstanceStatus(&status, instance)
	case lookup.State == "active":
		// The actor recorded the job as active a moment before its workflow's journal exists
		status.Status = StatusRunning
	default:
		return JobStatus{}, "", false, nil
	}

	return status, lookup.SubjectHash, true, nil
}

// Fill a job's status from its workflow's journal
func applyInstanceStatus(status *JobStatus, instance francisworkflow.InstanceStatus) {
	status.Status = StatusRunning
	status.RequestedAt = instance.CreatedAt
	status.StartedAt = instance.StartedAt
	status.CompletedAt = instance.CompletedAt

	for _, step := range instance.Steps {
		if step.Name != stepGenerate {
			continue
		}
		status.Total = step.Tasks
		status.Completed = step.Completed + step.Failed
		break
	}

	switch instance.Status {
	case francisworkflow.StatusCompleted:
		status.Status = StatusCompleted
	case francisworkflow.StatusFailed, francisworkflow.StatusCancelled:
		status.Status = StatusFailed
		status.Error = failureMessage(instance.Cause)
	}
}
