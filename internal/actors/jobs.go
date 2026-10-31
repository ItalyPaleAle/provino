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

// ErrImageBusy is returned for a job of an image that has another job running, or that an upload is still storing
// The job can be sent again once the image is free
var ErrImageBusy = errors.New("the image has another job running, or is still being uploaded")

// Jobs is what the API handlers use to start jobs and read their status
type Jobs interface {
	// Start starts a job's workflow and returns the job's ID, or ErrImageBusy
	Start(ctx context.Context, req JobRequest) (string, error)
	// Status returns a job's status, and the hash of the subject that started it when that's still known
	// The boolean is false when the image has no such job, or when its status has expired
	Status(ctx context.Context, namespace string, imageID string, jobID string) (JobStatus, string, bool, error)
	// ClaimUpload holds an image's ID for one upload that names it, identified by token, for at most ttl
	// It returns false when another upload holds the ID, or when a job of the image is running
	ClaimUpload(ctx context.Context, namespace string, imageID string, token string, ttl time.Duration) (bool, error)
	// ReleaseUpload gives up the claim the upload identified by token holds, which does nothing when it holds none
	ReleaseUpload(ctx context.Context, namespace string, imageID string, token string) error
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
	// The input is stored in UTC, like every time provino records
	// A request the image actor kept in its state comes back from Francis in the local zone, so it's converted rather than trusted
	if req.RequestedAt.IsZero() {
		req.RequestedAt = time.Now()
	}
	req.RequestedAt = req.RequestedAt.UTC()
	if req.Source != nil {
		source := *req.Source
		source.StoredAt = source.StoredAt.UTC()
		req.Source = &source
	}

	_, created, err := s.workflow.Start(ctx, req,
		francisworkflow.WithInstanceID(jobInstanceID(req.Namespace, req.ImageID, req.JobID)),
	)
	if err != nil {
		return fmt.Errorf("failed to start the workflow of job '%s': %w", req.JobID, err)
	}
	if created {
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

func (h *hostJobs) Start(ctx context.Context, req JobRequest) (string, error) {
	res, err := h.service.Invoke(ctx, imageActorType, imageActorID(req.Namespace, req.ImageID), methodStart, req)
	if err != nil {
		return "", fmt.Errorf("failed to start the job: %w", err)
	}

	var result startResult
	err = res.Decode(&result)
	if err != nil {
		return "", fmt.Errorf("failed to decode the started job: %w", err)
	}
	if result.Busy {
		return "", ErrImageBusy
	}

	return result.JobID, nil
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
	// The image's actor knows who started the active job and the recent ones
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
	// Francis reports the journal's times in the local zone, and the status carries them in UTC
	status.Status = StatusRunning
	status.RequestedAt = instance.CreatedAt.UTC()
	status.StartedAt = instance.StartedAt.UTC()
	status.CompletedAt = instance.CompletedAt.UTC()

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
