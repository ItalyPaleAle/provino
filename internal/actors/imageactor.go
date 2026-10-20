package actors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"github.com/italypaleale/francis/actor"
)

// Methods of the image actor
const (
	methodEnqueue  = "enqueue"
	methodFinished = "finished"
	methodClaim    = "claim"
	methodRelease  = "release"
	methodJob      = "job"
	alarmCheck     = "check"
)

// Bounds on the jobs the actor remembers after they finish, which it keeps only so a browser can read the status of its own jobs
const (
	maxHistory = 50
	// A start that failed is retried this long after, so a briefly unavailable runtime doesn't leave the queue stuck until the next request
	retryStartAfter = 30 * time.Second
	// How often the actor looks at a running job, so one whose release never arrived holds up the jobs behind it for a minute rather than until its deadline
	activePollInterval = time.Minute
	// How long after its deadline an active job is checked, giving its unwind time to report back before the actor steps in
	activeCheckMargin = time.Minute
)

// imageState is the durable state of an image's queue
type imageState struct {
	// The job whose workflow is running, if any
	Active *activeJob `json:"active,omitempty"`
	// Jobs waiting for the active one to finish, in order
	Queue []queuedJob `json:"queue,omitempty"`
	// Jobs that finished recently, most recent last
	History []finishedJob `json:"history,omitempty"`
	// An upload that named this image's ID and is storing it right now
	Upload *uploadClaim `json:"upload,omitempty"`
}

// uploadClaim holds an image ID for the upload that is storing it, so a second upload naming the same ID can't store it at the same time
type uploadClaim struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// claimRequest asks for an image's ID on behalf of one upload
type claimRequest struct {
	// Token that identifies the upload, which it presents again to release the claim
	Token string `json:"token"`
	// How long the claim holds when the upload never releases it, such as when its process stops
	TTL time.Duration `json:"ttl"`
}

// claimResult is the actor's answer to a claim
type claimResult struct {
	Claimed bool `json:"claimed"`
}

type activeJob struct {
	JobID       string    `json:"jobId"`
	SubjectHash string    `json:"subjectHash,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	// The job's request, kept until its workflow is known to exist, so a start that never happened can be made again
	Request *JobRequest `json:"request,omitempty"`
}

type queuedJob struct {
	JobID       string     `json:"jobId"`
	SubjectHash string     `json:"subjectHash,omitempty"`
	Request     JobRequest `json:"request"`
	EnqueuedAt  time.Time  `json:"enqueuedAt"`
}

type finishedJob struct {
	JobID       string    `json:"jobId"`
	SubjectHash string    `json:"subjectHash,omitempty"`
	FinishedAt  time.Time `json:"finishedAt"`
}

// enqueueResult is the actor's answer to an enqueue
type enqueueResult struct {
	JobID string `json:"jobId"`
	// 0 when the job started right away, its 1-based place in the queue otherwise
	Position int `json:"position"`
	// True when the queue was full and the job wasn't accepted
	QueueFull bool `json:"queueFull,omitempty"`
}

// finishedRequest tells the actor a job's workflow has finished
type finishedRequest struct {
	JobID string `json:"jobId"`
}

// finishedResult is the actor's answer to a finished job
type finishedResult struct {
	// When the actor recorded the job as finished, which a repeated report gets again, or zero when the actor no longer remembers the job
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// jobLookup is what the actor knows about one job
type jobLookup struct {
	Found bool `json:"found"`
	// One of "active", "queued", "finished"
	State       string `json:"state,omitempty"`
	Position    int    `json:"position,omitempty"`
	SubjectHash string `json:"subjectHash,omitempty"`
}

// Address the queue actor of one image
// The namespace can't contain a colon, so the two parts never run into each other
func imageActorID(namespace string, imageID string) string {
	return namespace + ":" + imageID
}

// Name the workflow instance of a job
// The instance is bound to its image, so a status lookup built from a URL can only ever find a job under the image it belongs to
func jobInstanceID(namespace string, imageID string, jobID string) string {
	return namespace + ":" + imageID + ":" + jobID
}

// imageActor serializes the jobs of one image: it holds them in a queue and starts each one's workflow when the previous one finishes
//
// It holds the queue itself, rather than starting every job as a workflow that waits for its turn, because Francis counts a workflow's waiting time against its deadline and drops an event raised before the workflow is waiting for it
type imageActor struct {
	namespace string
	imageID   string
	client    actor.Client[imageState]
	runtime   *Runtime
	jobs      *jobService
}

// Create the factory Francis uses to activate an image actor
func newImageActorFactory(rt *Runtime, jobs *jobService) actor.Factory {
	return func(actorID string, service *actor.Service) actor.Actor {
		namespace, imageID, _ := strings.Cut(actorID, ":")
		return &imageActor{
			namespace: namespace,
			imageID:   imageID,
			client:    actor.NewActorClient[imageState](imageActorType, actorID, service),
			runtime:   rt,
			jobs:      jobs,
		}
	}
}

// Invoke handles the methods that change the queue
func (a *imageActor) Invoke(ctx context.Context, method string, data actor.Envelope) (any, error) {
	switch method {
	case methodEnqueue:
		var req JobRequest
		err := data.Decode(&req)
		if err != nil {
			return nil, fmt.Errorf("failed to decode the job: %w", err)
		}
		return a.enqueue(ctx, req)

	case methodFinished:
		var req finishedRequest
		err := data.Decode(&req)
		if err != nil {
			return nil, fmt.Errorf("failed to decode the finished job: %w", err)
		}
		return a.finished(ctx, req.JobID)

	case methodClaim:
		var req claimRequest
		err := data.Decode(&req)
		if err != nil {
			return nil, fmt.Errorf("failed to decode the claim: %w", err)
		}
		return a.claim(ctx, req)

	case methodRelease:
		var req claimRequest
		err := data.Decode(&req)
		if err != nil {
			return nil, fmt.Errorf("failed to decode the release: %w", err)
		}
		return struct{}{}, a.release(ctx, req.Token)

	default:
		return nil, fmt.Errorf("unknown method '%s'", method)
	}
}

// Peek reports what the actor knows about a job, without changing anything
func (a *imageActor) Peek(ctx context.Context, method string, data actor.Envelope) (any, error) {
	if method != methodJob {
		return nil, fmt.Errorf("unknown method '%s'", method)
	}

	var req finishedRequest
	err := data.Decode(&req)
	if err != nil {
		return nil, fmt.Errorf("failed to decode the job: %w", err)
	}

	state, err := a.client.GetState(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read the queue: %w", err)
	}

	if state.Active != nil && state.Active.JobID == req.JobID {
		return jobLookup{Found: true, State: "active", SubjectHash: state.Active.SubjectHash}, nil
	}
	for i, job := range state.Queue {
		if job.JobID == req.JobID {
			return jobLookup{Found: true, State: "queued", Position: i + 1, SubjectHash: job.SubjectHash}, nil
		}
	}
	for _, job := range state.History {
		if job.JobID == req.JobID {
			return jobLookup{Found: true, State: "finished", SubjectHash: job.SubjectHash}, nil
		}
	}

	return jobLookup{}, nil
}

// Alarm is the actor's safety net: it frees the queue when an active job ended without telling it, retries a start that failed, and prunes the history
func (a *imageActor) Alarm(ctx context.Context, _ string, _ actor.Envelope) error {
	state, err := a.client.GetState(ctx)
	if err != nil {
		return fmt.Errorf("failed to read the queue: %w", err)
	}

	err = a.heal(ctx, &state)
	if err != nil {
		return err
	}

	// A failed start is retried here, and its failure is logged rather than returned, because the alarm rescheduled below retries it again
	err = a.startNext(ctx, &state)
	if err != nil {
		a.runtime.Log.WarnContext(ctx, "Failed to start the next job of an image", slog.String("namespace", a.namespace), slog.String("imageId", a.imageID), slog.Any("error", err))
	}

	return a.save(ctx, &state)
}

// Add a job to the queue, starting it right away when nothing else of the image is running
func (a *imageActor) enqueue(ctx context.Context, req JobRequest) (enqueueResult, error) {
	state, err := a.client.GetState(ctx)
	if err != nil {
		return enqueueResult{}, fmt.Errorf("failed to read the queue: %w", err)
	}

	// An active job that ended without telling the actor would hold the queue forever, so every enqueue checks on it
	err = a.heal(ctx, &state)
	if err != nil {
		return enqueueResult{}, err
	}

	if len(state.Queue) >= a.runtime.MaxQueuedJobs {
		err = a.save(ctx, &state)
		if err != nil {
			return enqueueResult{}, err
		}
		return enqueueResult{QueueFull: true}, nil
	}

	// The job ID is a v7, so the jobs of an image list in the order they were queued
	jobID := uuid.NewV7().String()
	req.JobID = jobID
	state.Queue = append(state.Queue, queuedJob{JobID: jobID, SubjectHash: req.SubjectHash, Request: req, EnqueuedAt: time.Now()})

	// A job that can't be started isn't accepted, so the caller learns now rather than finding a job that never runs
	err = a.startNext(ctx, &state)
	if err != nil {
		state.Queue = removeQueued(state.Queue, jobID)
		saveErr := a.save(ctx, &state)
		return enqueueResult{}, errors.Join(err, saveErr)
	}

	err = a.save(ctx, &state)
	if err != nil {
		return enqueueResult{}, err
	}

	result := enqueueResult{JobID: jobID}
	for i, job := range state.Queue {
		if job.JobID == jobID {
			result.Position = i + 1
		}
	}
	return result, nil
}

// Hold the image's ID for an upload that names it, unless another upload holds it or a job of the image is queued or running
//
// The claim is what makes checking that the ID is unused and then storing the image safe: the actor answers one call at a time, so between the two no other upload can claim the same ID
// A queued or running job means the image is already in use, and an upload writing its manifest now would race that job's own
func (a *imageActor) claim(ctx context.Context, req claimRequest) (claimResult, error) {
	if req.Token == "" || req.TTL <= 0 {
		return claimResult{}, errors.New("a claim needs a token and a positive TTL")
	}

	state, err := a.client.GetState(ctx)
	if err != nil {
		return claimResult{}, fmt.Errorf("failed to read the queue: %w", err)
	}

	// An active job that ended without telling the actor would otherwise hold the ID forever
	err = a.heal(ctx, &state)
	if err != nil {
		return claimResult{}, err
	}

	now := time.Now()
	heldByOther := state.Upload != nil && state.Upload.Token != req.Token && now.Before(state.Upload.ExpiresAt)
	if heldByOther || state.Active != nil || len(state.Queue) > 0 {
		return claimResult{Claimed: false}, a.save(ctx, &state)
	}

	state.Upload = &uploadClaim{Token: req.Token, ExpiresAt: now.Add(req.TTL)}
	err = a.save(ctx, &state)
	if err != nil {
		return claimResult{}, err
	}
	return claimResult{Claimed: true}, nil
}

// Give up the claim an upload holds, once its job is queued or the upload failed
// A release from an upload that doesn't hold the claim, such as one whose claim expired, changes nothing
func (a *imageActor) release(ctx context.Context, token string) error {
	state, err := a.client.GetState(ctx)
	if err != nil {
		return fmt.Errorf("failed to read the queue: %w", err)
	}

	if state.Upload == nil || state.Upload.Token != token {
		return nil
	}
	state.Upload = nil
	return a.save(ctx, &state)
}

// Record that a job finished, and start the next one
// It returns when the job was recorded as finished, which is the same for every report about it, so a job can use it as a timestamp that doesn't change across its retries
func (a *imageActor) finished(ctx context.Context, jobID string) (finishedResult, error) {
	state, err := a.client.GetState(ctx)
	if err != nil {
		return finishedResult{}, fmt.Errorf("failed to read the queue: %w", err)
	}

	// A job reports at least once, so a report about a job that isn't active anymore is a repeat and changes nothing
	if state.Active == nil || state.Active.JobID != jobID {
		for _, job := range state.History {
			if job.JobID == jobID {
				return finishedResult{FinishedAt: job.FinishedAt}, nil
			}
		}
		return finishedResult{}, nil
	}
	finishedAt := a.finishActive(&state)

	err = a.startNext(ctx, &state)
	if err != nil {
		// The alarm retries the start, so the job that finished is still recorded as finished
		a.runtime.Log.WarnContext(ctx, "Failed to start the next job of an image", slog.String("namespace", a.namespace), slog.String("imageId", a.imageID), slog.Any("error", err))
	}

	err = a.save(ctx, &state)
	if err != nil {
		return finishedResult{}, err
	}
	return finishedResult{FinishedAt: finishedAt}, nil
}

// Free the queue when the active job's workflow has ended, and start it when it never did
func (a *imageActor) heal(ctx context.Context, state *imageState) error {
	if state.Active == nil {
		return nil
	}

	status, found, err := a.jobs.instanceStatus(ctx, a.namespace, a.imageID, state.Active.JobID)
	if err != nil {
		return fmt.Errorf("failed to check on the active job: %w", err)
	}
	if found {
		// The workflow exists, so the request isn't needed to start it again
		state.Active.Request = nil
		if !isTerminal(status) {
			return nil
		}
	}

	// A workflow that doesn't exist was never started when its journal can't have been purged yet, which is what a process stopping between recording the job and starting it leaves behind
	// Starting it now is safe, because a start that did happen after all is found by its job ID and discarded
	if !found && state.Active.Request != nil && !a.couldBePurged(state.Active, time.Now()) {
		err = a.jobs.start(ctx, *state.Active.Request)
		if err != nil {
			// The job stays active, and the next check tries again
			a.runtime.Log.WarnContext(ctx, "Failed to start an active job whose workflow never started", slog.String("namespace", a.namespace), slog.String("imageId", a.imageID), slog.String("jobId", state.Active.JobID), slog.Any("error", err))
			return nil
		}
		a.runtime.Log.WarnContext(ctx, "Started an active job whose workflow never started", slog.String("namespace", a.namespace), slog.String("imageId", a.imageID), slog.String("jobId", state.Active.JobID))
		state.Active.Request = nil
		return nil
	}

	a.runtime.Log.WarnContext(ctx, "Freed the queue of an image whose active job ended without reporting back",
		slog.String("namespace", a.namespace),
		slog.String("imageId", a.imageID),
		slog.String("jobId", state.Active.JobID),
		slog.Bool("found", found),
	)
	a.finishActive(state)
	return nil
}

// Report whether the journal of an active job's workflow could have been purged by now
// It can only be purged once the retention has passed after the workflow completed, which is after the job became active
func (a *imageActor) couldBePurged(active *activeJob, now time.Time) bool {
	retention := a.runtime.CompletedWorkflowRetention
	return retention > 0 && now.Sub(active.StartedAt) >= retention
}

// Move the active job to the history, returning when it finished
func (a *imageActor) finishActive(state *imageState) time.Time {
	finishedAt := time.Now().UTC()
	state.History = append(state.History, finishedJob{
		JobID:       state.Active.JobID,
		SubjectHash: state.Active.SubjectHash,
		FinishedAt:  finishedAt,
	})
	if len(state.History) > maxHistory {
		state.History = state.History[len(state.History)-maxHistory:]
	}
	state.Active = nil
	return finishedAt
}

// Start the job at the head of the queue when nothing is active
//
// The job is recorded as active before its workflow is started, so a crash in between leaves an active job whose workflow doesn't exist, which heal frees, rather than a running workflow the actor doesn't know about, which nothing would stop the next job from racing
func (a *imageActor) startNext(ctx context.Context, state *imageState) error {
	if state.Active != nil || len(state.Queue) == 0 {
		return nil
	}

	// The check is scheduled before the job is recorded as active, so a process that stops between the two leaves an alarm behind to find the job
	err := a.client.SetAlarm(ctx, alarmCheck, actor.AlarmProperties{DueTime: time.Now().Add(retryStartAfter)})
	if err != nil {
		return fmt.Errorf("failed to schedule the queue check: %w", err)
	}

	next := state.Queue[0]
	state.Queue = state.Queue[1:]
	request := next.Request
	state.Active = &activeJob{JobID: next.JobID, SubjectHash: next.SubjectHash, StartedAt: time.Now(), Request: &request}
	err = a.client.SetState(ctx, *state, nil)
	if err != nil {
		state.Queue = append([]queuedJob{next}, state.Queue...)
		state.Active = nil
		return fmt.Errorf("failed to record the active job: %w", err)
	}

	err = a.jobs.start(ctx, next.Request)
	if err != nil {
		// The job goes back to the head of the queue, so the order is kept when the start is retried
		state.Queue = append([]queuedJob{next}, state.Queue...)
		state.Active = nil
		return fmt.Errorf("failed to start the job: %w", err)
	}

	// The workflow exists now, so its request no longer needs to be kept
	state.Active.Request = nil
	return nil
}

// Persist the state and schedule the next check, or delete both once there's nothing left to remember
func (a *imageActor) save(ctx context.Context, state *imageState) error {
	now := time.Now()
	a.pruneHistory(state, now)

	// A claim whose upload never released it stops holding the ID once it expires
	if state.Upload != nil && !now.Before(state.Upload.ExpiresAt) {
		state.Upload = nil
	}

	// An image with nothing queued and nothing to remember leaves nothing behind, so the actors of every image ever uploaded don't accumulate
	if state.Active == nil && len(state.Queue) == 0 && len(state.History) == 0 && state.Upload == nil {
		err := a.client.DeleteAlarm(ctx, alarmCheck)
		if err != nil && !errors.Is(err, actor.ErrAlarmNotFound) {
			return fmt.Errorf("failed to delete the alarm: %w", err)
		}
		err = a.client.DeleteState(ctx)
		if err != nil && !errors.Is(err, actor.ErrStateNotFound) {
			return fmt.Errorf("failed to delete the queue: %w", err)
		}
		return nil
	}

	err := a.client.SetState(ctx, *state, nil)
	if err != nil {
		return fmt.Errorf("failed to save the queue: %w", err)
	}

	err = a.client.SetAlarm(ctx, alarmCheck, actor.AlarmProperties{DueTime: a.nextCheck(state, now)})
	if err != nil {
		return fmt.Errorf("failed to schedule the queue check: %w", err)
	}

	return nil
}

// Work out when the actor next needs to look at its queue
func (a *imageActor) nextCheck(state *imageState, now time.Time) time.Time {
	var due time.Time
	consider := func(t time.Time) {
		if due.IsZero() || t.Before(due) {
			due = t
		}
	}

	switch {
	case state.Active != nil:
		// The workflow is failed by its own deadline, so past that it should have reported back
		// Before then it's looked at every so often, so one that ended without its release reaching the actor doesn't hold up the queue until that deadline
		consider(state.Active.StartedAt.Add(jobDeadline(a.runtime.WorkflowTimeout) + activeCheckMargin))
		consider(now.Add(activePollInterval))
	case len(state.Queue) > 0:
		// A queue with nothing active is a start that failed
		consider(now.Add(retryStartAfter))
	}
	if len(state.History) > 0 && a.runtime.CompletedWorkflowRetention > 0 {
		consider(state.History[0].FinishedAt.Add(a.runtime.CompletedWorkflowRetention))
	}
	if state.Upload != nil {
		// An expired claim is cleared so the actor's state can be deleted
		consider(state.Upload.ExpiresAt)
	}

	// Nothing above applies to a history kept indefinitely, which still gets an occasional look
	if due.IsZero() {
		due = now.Add(24 * time.Hour)
	}
	return due
}

// Drop the finished jobs whose status has expired along with their workflow's journal
func (a *imageActor) pruneHistory(state *imageState, now time.Time) {
	retention := a.runtime.CompletedWorkflowRetention
	if retention <= 0 {
		return
	}

	keep := state.History[:0]
	for _, job := range state.History {
		if now.Sub(job.FinishedAt) < retention {
			keep = append(keep, job)
		}
	}
	state.History = keep
}

func removeQueued(queue []queuedJob, jobID string) []queuedJob {
	out := queue[:0]
	for _, job := range queue {
		if job.JobID != jobID {
			out = append(out, job)
		}
	}
	return out
}
