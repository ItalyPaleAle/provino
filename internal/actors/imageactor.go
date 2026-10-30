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
	methodStart    = "start"
	methodFinished = "finished"
	methodClaim    = "claim"
	methodRelease  = "release"
	methodJob      = "job"
	alarmCheck     = "check"
)

// Bounds on the jobs the actor remembers after they finish, which it keeps only so a browser can read the status of its own jobs
const (
	maxHistory = 50
	// How soon the actor checks on a job it's about to start, so a process that stops before the workflow starts leaves an alarm behind to start it
	checkStartAfter = 30 * time.Second
	// How often the actor looks at a running job, so one whose release never arrived holds the image for a minute rather than until its deadline
	activePollInterval = time.Minute
	// How long after its deadline an active job is checked, giving its unwind time to report back before the actor steps in
	activeCheckMargin = time.Minute
)

// imageState is the durable state of an image's actor
type imageState struct {
	// The job whose workflow is running, if any
	Active *activeJob `json:"active,omitempty"`
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
	// How long finished journals were retained when the job became active, zero for indefinitely
	// The retention can change across restarts, and the journal could have been purged under either value, so heal goes by the shorter of this one and the current one
	Retention time.Duration `json:"retention,omitempty"`
}

type finishedJob struct {
	JobID       string    `json:"jobId"`
	SubjectHash string    `json:"subjectHash,omitempty"`
	FinishedAt  time.Time `json:"finishedAt"`
}

// startResult is the actor's answer to a start
type startResult struct {
	JobID string `json:"jobId"`
	// True when another job of the image is running, or an upload is still storing it, so the job wasn't accepted
	Busy bool `json:"busy,omitempty"`
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
	// One of "active", "finished"
	State       string `json:"state,omitempty"`
	SubjectHash string `json:"subjectHash,omitempty"`
}

// Address the actor of one image
// The namespace can't contain a colon, so the two parts never run into each other
func imageActorID(namespace string, imageID string) string {
	return namespace + ":" + imageID
}

// Name the workflow instance of a job
// The instance is bound to its image, so a status lookup built from a URL can only ever find a job under the image it belongs to
func jobInstanceID(namespace string, imageID string, jobID string) string {
	return namespace + ":" + imageID + ":" + jobID
}

// imageActor runs the jobs of one image one at a time: it starts a job's workflow only when no other job of the image is running, and refuses the job otherwise
//
// Every job writes the image's manifest, and a failed one puts back the manifest it found, so two jobs of one image running at once would undo each other's work
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

// Invoke handles the methods that change the image's state
func (a *imageActor) Invoke(ctx context.Context, method string, data actor.Envelope) (any, error) {
	switch method {
	case methodStart:
		var req JobRequest
		err := data.Decode(&req)
		if err != nil {
			return nil, fmt.Errorf("failed to decode the job: %w", err)
		}
		return a.startJob(ctx, req)

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
		return nil, fmt.Errorf("failed to read the image's state: %w", err)
	}

	if state.Active != nil && state.Active.JobID == req.JobID {
		return jobLookup{Found: true, State: "active", SubjectHash: state.Active.SubjectHash}, nil
	}
	for _, job := range state.History {
		if job.JobID == req.JobID {
			return jobLookup{Found: true, State: "finished", SubjectHash: job.SubjectHash}, nil
		}
	}

	return jobLookup{}, nil
}

// Alarm is the actor's safety net: it frees the image when its active job ended without telling it, starts an active job whose workflow never started, and prunes the history
func (a *imageActor) Alarm(ctx context.Context, _ string, _ actor.Envelope) error {
	state, err := a.client.GetState(ctx)
	if err != nil {
		return fmt.Errorf("failed to read the image's state: %w", err)
	}

	err = a.heal(ctx, &state)
	if err != nil {
		return err
	}

	return a.save(ctx, &state)
}

// Start a job's workflow, unless another job of the image is running or an upload is still storing the image
func (a *imageActor) startJob(ctx context.Context, req JobRequest) (startResult, error) {
	state, err := a.client.GetState(ctx)
	if err != nil {
		return startResult{}, fmt.Errorf("failed to read the image's state: %w", err)
	}

	// An active job that ended without telling the actor would hold the image forever, so every start checks on it
	err = a.heal(ctx, &state)
	if err != nil {
		return startResult{}, err
	}

	// Only one job of an image runs at a time, so a job sent while another is running is refused rather than kept, and the caller sends it again later
	// An upload holding the ID is still storing the original, so the only job that may start is the upload's own, which carries what it learned about the original; any other would fail with image_not_found
	uploading := state.Upload != nil && time.Now().Before(state.Upload.ExpiresAt)
	if state.Active != nil || (uploading && req.Source == nil) {
		err = a.save(ctx, &state)
		if err != nil {
			return startResult{}, err
		}
		return startResult{Busy: true}, nil
	}

	// The job ID is a v7, so the jobs of an image list in the order they started
	req.JobID = uuid.NewV7().String()

	// A job that can't be started isn't accepted, so the caller learns now rather than finding a job that never runs
	// The state is saved even then, because a start that failed may still leave the job active until heal confirms its workflow doesn't exist
	err = a.activate(ctx, &state, req)
	if err != nil {
		saveErr := a.save(ctx, &state)
		return startResult{}, errors.Join(err, saveErr)
	}

	err = a.save(ctx, &state)
	if err != nil {
		return startResult{}, err
	}
	return startResult{JobID: req.JobID}, nil
}

// Hold the image's ID for an upload that names it, unless another upload holds it or a job of the image is running
//
// The claim is what makes checking that the ID is unused and then storing the image safe: the actor answers one call at a time, so between the two no other upload can claim the same ID
// A running job means the image is already in use, and an upload writing its manifest now would race that job's own
func (a *imageActor) claim(ctx context.Context, req claimRequest) (claimResult, error) {
	if req.Token == "" || req.TTL <= 0 {
		return claimResult{}, errors.New("a claim needs a token and a positive TTL")
	}

	state, err := a.client.GetState(ctx)
	if err != nil {
		return claimResult{}, fmt.Errorf("failed to read the image's state: %w", err)
	}

	// An active job that ended without telling the actor would otherwise hold the ID forever
	err = a.heal(ctx, &state)
	if err != nil {
		return claimResult{}, err
	}

	now := time.Now()
	heldByOther := state.Upload != nil && state.Upload.Token != req.Token && now.Before(state.Upload.ExpiresAt)
	if heldByOther || state.Active != nil {
		return claimResult{Claimed: false}, a.save(ctx, &state)
	}

	state.Upload = &uploadClaim{Token: req.Token, ExpiresAt: now.Add(req.TTL)}
	err = a.save(ctx, &state)
	if err != nil {
		return claimResult{}, err
	}
	return claimResult{Claimed: true}, nil
}

// Give up the claim an upload holds, once its job has started or the upload failed
// A release from an upload that doesn't hold the claim, such as one whose claim expired, changes nothing
func (a *imageActor) release(ctx context.Context, token string) error {
	state, err := a.client.GetState(ctx)
	if err != nil {
		return fmt.Errorf("failed to read the image's state: %w", err)
	}

	if state.Upload == nil || state.Upload.Token != token {
		return nil
	}
	state.Upload = nil
	return a.save(ctx, &state)
}

// Record that a job finished, which frees the image for its next job
// It returns when the job was recorded as finished, which is the same for every report about it, so a job can use it as a timestamp that doesn't change across its retries
func (a *imageActor) finished(ctx context.Context, jobID string) (finishedResult, error) {
	state, err := a.client.GetState(ctx)
	if err != nil {
		return finishedResult{}, fmt.Errorf("failed to read the image's state: %w", err)
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

	err = a.save(ctx, &state)
	if err != nil {
		return finishedResult{}, err
	}
	return finishedResult{FinishedAt: finishedAt}, nil
}

// Free the image when the active job's workflow has ended, and start the workflow when it never did
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

	a.runtime.Log.WarnContext(ctx, "Freed an image whose active job ended without reporting back",
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
//
// The retention may have changed since the job became active, so the shorter of the two values counts: raising it doesn't bring back a journal the old value already purged
// A zero retention keeps journals indefinitely, so it never counts as the shorter one; the job recorded zero too when it became active before the retention was recorded at all
func (a *imageActor) couldBePurged(active *activeJob, now time.Time) bool {
	retention := a.runtime.CompletedWorkflowRetention
	if active.Retention > 0 && (retention <= 0 || active.Retention < retention) {
		retention = active.Retention
	}
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

// Record a job as active and start its workflow
//
// The job is recorded as active before its workflow is started, so a crash in between leaves an active job whose workflow doesn't exist, which heal starts or frees, rather than a running workflow the actor doesn't know about, which nothing would stop another job from racing
func (a *imageActor) activate(ctx context.Context, state *imageState, req JobRequest) error {
	// The check is scheduled before the job is recorded as active, so a process that stops between the two leaves an alarm behind to find the job
	err := a.client.SetAlarm(ctx, alarmCheck, actor.AlarmProperties{DueTime: time.Now().Add(checkStartAfter)})
	if err != nil {
		return fmt.Errorf("failed to schedule the image's check: %w", err)
	}

	state.Active = &activeJob{JobID: req.JobID, SubjectHash: req.SubjectHash, StartedAt: time.Now(), Request: &req, Retention: a.runtime.CompletedWorkflowRetention}
	err = a.client.SetState(ctx, *state, nil)
	if err != nil {
		state.Active = nil
		return fmt.Errorf("failed to record the active job: %w", err)
	}

	// The request is dropped whatever the outcome: a workflow that started no longer needs it, and a job whose start failed is never started again, since its caller is told it wasn't accepted
	// A failed start still leaves the job active, because Francis may have recorded the start before the failure, such as when its answer timed out
	// The next heal then keeps the image busy if the workflow exists and frees it otherwise, so a workflow that did start never runs alongside another job of the image
	err = a.jobs.start(ctx, req)
	state.Active.Request = nil
	if err != nil {
		return fmt.Errorf("failed to start the job: %w", err)
	}
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

	// An image with nothing running and nothing to remember leaves nothing behind, so the actors of every image ever uploaded don't accumulate
	if state.Active == nil && len(state.History) == 0 && state.Upload == nil {
		err := a.client.DeleteAlarm(ctx, alarmCheck)
		if err != nil && !errors.Is(err, actor.ErrAlarmNotFound) {
			return fmt.Errorf("failed to delete the alarm: %w", err)
		}
		err = a.client.DeleteState(ctx)
		if err != nil && !errors.Is(err, actor.ErrStateNotFound) {
			return fmt.Errorf("failed to delete the image's state: %w", err)
		}
		return nil
	}

	err := a.client.SetState(ctx, *state, nil)
	if err != nil {
		return fmt.Errorf("failed to save the image's state: %w", err)
	}

	err = a.client.SetAlarm(ctx, alarmCheck, actor.AlarmProperties{DueTime: a.nextCheck(state, now)})
	if err != nil {
		return fmt.Errorf("failed to schedule the image's check: %w", err)
	}

	return nil
}

// Work out when the actor next needs to look at its image
func (a *imageActor) nextCheck(state *imageState, now time.Time) time.Time {
	var due time.Time
	consider := func(t time.Time) {
		if due.IsZero() || t.Before(due) {
			due = t
		}
	}

	if state.Active != nil {
		// The workflow is failed by its own deadline, so past that it should have reported back
		// Before then it's looked at every so often, so one that ended without its release reaching the actor doesn't hold the image until that deadline
		consider(state.Active.StartedAt.Add(jobDeadline(a.runtime.WorkflowTimeout) + activeCheckMargin))
		consider(now.Add(activePollInterval))
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
