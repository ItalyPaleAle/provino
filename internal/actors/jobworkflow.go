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
	francisworkflow "github.com/italypaleale/francis/builtin/workflow"
	"github.com/italypaleale/go-kit/utils"
	"go.opentelemetry.io/otel"

	"github.com/italypaleale/provino/imagekeys"
	"github.com/italypaleale/provino/internal/imaging"
	"github.com/italypaleale/provino/internal/notify"
	"github.com/italypaleale/provino/internal/store"
	"github.com/italypaleale/provino/manifest"
	"github.com/italypaleale/provino/webhook"
)

// How many times a failed job tries to deliver its failure event before giving up, so an unreachable endpoint can't hold the unwind
const failureDeliveryAttempts = 5

// jobWorkflowHandlers contains the executable functions in the workflow definition
// Francis invokes every function here on a workflow worker actor, never on the orchestrator
type jobWorkflowHandlers struct {
	runtime *Runtime
}

// variantIdentity names one stored file by format and fit, with either the box that requested it or the size it came out at
// Fit is part of it because a cover box and a contain box of the same size describe different pictures
type variantIdentity struct {
	Format imaging.Format
	Fit    imaging.Fit
	Width  int
	Height int
}

// storedVariants indexes the files a previous job stored for an image that this one may reuse
type storedVariants struct {
	// Files by the box, fit and format that requested them
	byBox map[variantIdentity]manifest.Variant
	// Files by the size they came out at, which is how a box the source can't fill finds a file another box stored with the same pixels
	byOutput map[variantIdentity]manifest.Variant
}

// The deadline of a whole job, which is a backstop over the per-attempt budget
func jobDeadline(workflowTimeout time.Duration) time.Duration {
	const multiplier = 6
	if workflowTimeout > time.Duration((1<<63-1)/multiplier) {
		return time.Duration(1<<63 - 1)
	}
	return workflowTimeout * multiplier
}

// NewJobWorkflow builds the Francis workflow every job runs
func NewJobWorkflow(rt *Runtime, concurrency int) (*francisworkflow.Workflow, error) {
	h := &jobWorkflowHandlers{runtime: rt}

	// Francis requires a finite retention, so this represents the configuration's indefinite value without risking duration overflow when Francis doubles it for the journal TTL
	retention := utils.PositiveOr(rt.CompletedWorkflowRetention, time.Duration(1<<62-1))

	// Attempt deadlines begin after a worker starts so queued work cannot consume the handler budget, and the instance deadline is the backstop for the whole job
	// The job waits for its turn in the image's queue before its workflow exists, so that wait doesn't count against either
	wf, err := francisworkflow.New(jobWorkflowName,
		francisworkflow.WithVersion(jobWorkflowVersion),
		francisworkflow.WithTimeout(jobDeadline(rt.WorkflowTimeout)),
		francisworkflow.WithRetention(francisworkflow.RetentionPolicy{
			Completed: retention,
			Failed:    retention,
			Cancelled: retention,
		}),
		francisworkflow.WithConcurrency(concurrency),
		francisworkflow.WithLogger(rt.Log),
		francisworkflow.WithMeter(otel.Meter("github.com/italypaleale/provino/internal/actors/workflow")),
		francisworkflow.WithOutput(stepManifest),
		francisworkflow.WithSteps(
			// The snapshot's compensation is the job's failure path, so it also runs when the snapshot itself fails, which is what a missing original does
			francisworkflow.Step(stepSnapshot,
				francisworkflow.WithRun(h.snapshot),
				francisworkflow.WithCompensate(h.cleanupFailed),
				francisworkflow.WithCompensateOnFailure(),
				francisworkflow.WithCompensateMaxAttempts(failureDeliveryAttempts+1),
				francisworkflow.WithAttemptTimeout(rt.WorkflowTimeout),
				francisworkflow.WithCompensateTimeout(rt.WorkflowTimeout),
			),
			francisworkflow.Step(stepPrepare,
				francisworkflow.WithRun(h.prepare),
				francisworkflow.WithAttemptTimeout(rt.WorkflowTimeout),
			),
			francisworkflow.ForEach(stepGenerate,
				francisworkflow.WithItemsFrom(stepPrepare),
				francisworkflow.WithInputFrom(stepSnapshot),
				francisworkflow.WithRun(h.generateVariant),
				francisworkflow.WithCompensate(h.deleteVariant),
				francisworkflow.WithFailurePolicy(francisworkflow.FailFast),
				francisworkflow.WithAttemptTimeout(rt.WorkflowTimeout),
				francisworkflow.WithCompensateTimeout(rt.WorkflowTimeout),
			),
			francisworkflow.Step(stepManifest,
				francisworkflow.WithRun(h.writeManifest),
				francisworkflow.WithCompensate(h.restoreManifest),
				francisworkflow.WithCompensateOnFailure(),
				francisworkflow.WithInputFrom(stepSnapshot, stepPrepare),
				francisworkflow.WithAttemptTimeout(rt.WorkflowTimeout),
				francisworkflow.WithCompensateTimeout(rt.WorkflowTimeout),
			),
			// Every step after the manifest is optional or can't fail, so a job whose variants are stored never unwinds them because a notification or a local cleanup went wrong
			francisworkflow.Step(stepNotify,
				francisworkflow.WithRun(h.notify),
				francisworkflow.WithOptional(),
				francisworkflow.WithAttemptTimeout(rt.WorkflowTimeout),
			),
			francisworkflow.Step(stepCleanup,
				francisworkflow.WithRun(h.cleanupCompleted),
				francisworkflow.WithInputFrom(stepManifest),
				francisworkflow.WithAttemptTimeout(rt.WorkflowTimeout),
			),
			francisworkflow.Step(stepRelease,
				francisworkflow.WithRun(h.release),
				francisworkflow.WithOptional(),
				francisworkflow.WithAttemptTimeout(rt.WorkflowTimeout),
			),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create the job workflow: %w", err)
	}

	return wf, nil
}

// Record what the image looks like before the job changes anything, and make sure its original exists
func (w *jobWorkflowHandlers) snapshot(ctx context.Context, task francisworkflow.Task) (any, error) {
	req, namespace, imageID, err := decodeRequest(task)
	if err != nil {
		return nil, err
	}

	snap := jobSnapshot{StartedAt: time.Now()}

	// The manifest is kept byte for byte, so a failed job can put back exactly what it found, even one written by an older version
	content, err := w.runtime.Store.ReadManifestConsistent(ctx, namespace, imageID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// An image without a manifest has nothing to put back
	case err != nil:
		return nil, fmt.Errorf("failed to read the existing manifest: %w", err)
	default:
		snap.Previous = content
	}
	previous := w.parsePrevious(ctx, snap.Previous, namespace, imageID)

	// What the upload measured is the freshest information about the original, then the manifest, and the request's format only when neither is there
	switch {
	case req.Source != nil:
		snap.SourceFormat = req.Source.Format
		snap.SourceName = req.Source.Name
		snap.SourceSize = req.Source.Size
		snap.SourceResolution = imaging.Resolution{Width: req.Source.Width, Height: req.Source.Height}
		snap.StoredAt = req.Source.StoredAt
	case previous != nil:
		snap.SourceFormat = previous.SourceFormat
		snap.SourceName = previous.SourceName
		snap.SourceSize = previous.SourceSize
		snap.SourceResolution = imaging.Resolution{Width: previous.SourceWidth, Height: previous.SourceHeight}
		snap.StoredAt = previous.StoredAt
	case req.SourceFormat != "":
		snap.SourceFormat = req.SourceFormat
	default:
		return nil, errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("%s: the image has no manifest, and the request names no sourceFormat to find its original by", ErrorImageNotFound))
	}

	// A job for an image that doesn't exist fails here, before anything is written, and doesn't spend its retries on it
	exists, err := w.runtime.Store.OriginalExists(ctx, namespace, imageID, snap.SourceFormat)
	if err != nil {
		return nil, fmt.Errorf("failed to look up the original: %w", err)
	}
	if !exists {
		return nil, errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("%s: there is no original for image '%s' in namespace '%s'", ErrorImageNotFound, imageID, namespace))
	}

	return snap, nil
}

// Plan the fan-out: learn the source's size, reuse stored variants, skip the boxes another variant already holds, and cache the source only when encoding remains
func (w *jobWorkflowHandlers) prepare(ctx context.Context, task francisworkflow.Task) (any, error) {
	req, namespace, imageID, err := decodeRequest(task)
	if err != nil {
		return nil, err
	}
	var snap jobSnapshot
	err = task.DecodeOutput(stepSnapshot, &snap)
	if err != nil {
		return nil, errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("failed to decode the snapshot: %w", err))
	}

	// A job with no variants only records the image, so there's nothing to plan
	if len(req.Variants) == 0 {
		return []variantTask{}, nil
	}

	// Every decision below depends on the source's size, because no variant may be larger than the source
	source := snap.SourceResolution
	if source.Width <= 0 || source.Height <= 0 {
		source, err = w.sourceSize(ctx, namespace, imageID, snap.SourceFormat)
		if err != nil {
			if !isRetryable(err) {
				return nil, errors.Join(actor.ErrJobPermanentFailure, err)
			}
			return nil, err
		}
	}

	previous := w.parsePrevious(ctx, snap.Previous, namespace, imageID)
	stored, err := w.reusableVariants(ctx, namespace, imageID, previous, source)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect existing variants: %w", err)
	}

	// A skipped box encodes nothing, a reused one reads the stored file's metadata, and only the rest needs the source
	tasks := make([]variantTask, len(req.Variants))
	skips := planSkips(source, req.Variants)
	reusedCount := 0
	skippedCount := 0
	needsSource := false
	for i, spec := range req.Variants {
		spec.Index = i
		tasks[i] = variantTask{
			Namespace:    namespace,
			ImageID:      imageID.String(),
			SourceFormat: snap.SourceFormat,
			Spec:         spec,
			SourceSize:   source,
		}
		if skips[i] != nil {
			tasks[i].Skip = skips[i]
			skippedCount++
			continue
		}
		existing, reusable := stored.lookup(source, spec)
		if reusable {
			tasks[i].Existing = &existing
			reusedCount++
		} else {
			needsSource = true
		}
	}

	// A cache miss only costs another download, so prefetching never prevents the durable fan-out from starting
	if needsSource {
		err = w.runtime.SourceCache.Prefetch(ctx, namespace, imageID, snap.SourceFormat)
		if err != nil {
			w.runtime.Log.WarnContext(ctx, "Failed to cache the source image locally - each missing variant will download it on its own", slog.Any("error", err))
		}
	}

	w.runtime.Log.InfoContext(ctx, "Job started",
		slog.String("jobId", req.JobID),
		slog.String("namespace", namespace),
		slog.String("imageId", imageID.String()),
		slog.String("source", source.String()),
		slog.Int("variants", len(tasks)),
		slog.Int("reused", reusedCount),
		slog.Int("skipped", skippedCount),
	)
	return tasks, nil
}

// Parse the manifest the snapshot found, returning nil when it isn't a provino/v1 manifest of this image
// An older or foreign document is still put back when a job fails, but nothing in it is trusted to describe stored files
func (w *jobWorkflowHandlers) parsePrevious(ctx context.Context, content []byte, namespace string, imageID uuid.UUID) *manifest.Manifest {
	if len(content) == 0 {
		return nil
	}

	m, err := manifest.Parse(content)
	if err != nil {
		w.runtime.Log.WarnContext(ctx, "Ignoring an existing manifest this version can't read", slog.String("imageId", imageID.String()), slog.Any("error", err))
		return nil
	}
	if m.Namespace != namespace || m.ImageID != imageID.String() {
		return nil
	}

	return &m
}

// Learn the size of the source after its EXIF orientation from the source's header
func (w *jobWorkflowHandlers) sourceSize(ctx context.Context, namespace string, imageID uuid.UUID, format string) (imaging.Resolution, error) {
	// Reading through the cache also leaves the source on disk for the variants that still need encoding
	source, err := w.runtime.SourceCache.Read(ctx, namespace, imageID, format)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrTooLarge) {
		return imaging.Resolution{}, fmt.Errorf("%w: %w", imaging.ErrSourceUnreadable, err)
	} else if err != nil {
		return imaging.Resolution{}, fmt.Errorf("failed to read the source image: %w", err)
	}

	size, err := w.runtime.Encoder.SourceSize(ctx, source)
	if err != nil {
		return imaging.Resolution{}, fmt.Errorf("failed to read the size of the source image: %w", err)
	}
	return size, nil
}

// Index the files of the previous manifest that still exist and are no larger than the source, which are the ones this job may reuse
func (w *jobWorkflowHandlers) reusableVariants(ctx context.Context, namespace string, imageID uuid.UUID, previous *manifest.Manifest, source imaging.Resolution) (storedVariants, error) {
	stored := storedVariants{
		byBox:    make(map[variantIdentity]manifest.Variant),
		byOutput: make(map[variantIdentity]manifest.Variant),
	}
	if previous == nil {
		return stored, nil
	}

	for _, entry := range previous.Variants {
		// A skipped entry repeats the file of another entry rather than owning one
		if entry.Skipped || !entry.Succeeded || entry.Width <= 0 || entry.Height <= 0 || entry.Size <= 0 || (entry.Resolution.Width <= 0 && entry.Resolution.Height <= 0) {
			continue
		}

		// A file larger than its source was enlarged before provino stopped doing that, so it's encoded again at the source's own size
		if entry.Width > source.Width || entry.Height > source.Height {
			continue
		}

		exists, existsErr := w.runtime.Store.VariantExists(ctx, namespace, imageID, entry.Width, entry.Height, entry.Format.Extension())
		if existsErr != nil {
			return storedVariants{}, fmt.Errorf("inspect existing variant %s: %w", entry.Name, existsErr)
		}
		if !exists {
			continue
		}

		fit := entry.Fit.OrDefault()
		stored.byBox[variantIdentity{Format: entry.Format, Fit: fit, Width: entry.Resolution.Width, Height: entry.Resolution.Height}] = entry
		stored.byOutput[variantIdentity{Format: entry.Format, Fit: fit, Width: entry.Width, Height: entry.Height}] = entry
	}
	return stored, nil
}

// Find a stored file for a variant: the one its own box requested, or, for a box the source can't fill, any file with the same full-resolution pixels
// Quality and effort aren't compared, so a stored file is reused even when the encoder settings have changed since
func (s storedVariants) lookup(source imaging.Resolution, spec VariantSpec) (manifest.Variant, bool) {
	fit := spec.Fit.OrDefault()
	full, isFull := imaging.FullResolution(source, spec.Resolution, fit)

	entry, found := s.byBox[variantIdentity{Format: spec.Format, Fit: fit, Width: spec.Resolution.Width, Height: spec.Resolution.Height}]
	if found && (!isFull || (entry.Width == full.Width && entry.Height == full.Height)) {
		return entry, true
	}
	if !isFull {
		return manifest.Variant{}, false
	}

	// Any file of this fit and format at the full-resolution size holds the same pixels, whichever box asked for it
	entry, found = s.byOutput[variantIdentity{Format: spec.Format, Fit: fit, Width: full.Width, Height: full.Height}]
	return entry, found
}

// Plan which variants are skipped because a tighter box of the same fit and format already holds their pixels
// A box the source can't fill gets the source's own pixels, so every such box of one fit and format comes out the same: only the tightest stores a file, and the others point at it
func planSkips(source imaging.Resolution, specs []VariantSpec) []*SkippedVariant {
	skips := make([]*SkippedVariant, len(specs))

	// Group the boxes the source can't fill by the file they'd produce, and keep the tightest box of each group, or the first on a tie
	groups := make([]*variantIdentity, len(specs))
	holders := make(map[variantIdentity]int, len(specs))
	for i, spec := range specs {
		fit := spec.Fit.OrDefault()
		full, isFull := imaging.FullResolution(source, spec.Resolution, fit)
		if !isFull {
			continue
		}

		group := variantIdentity{Format: spec.Format, Fit: fit, Width: full.Width, Height: full.Height}
		groups[i] = &group
		holder, found := holders[group]
		if !found || imaging.ScaleFactor(source, spec.Resolution, fit) < imaging.ScaleFactor(source, specs[holder].Resolution, fit) {
			holders[group] = i
		}
	}

	// Every other box of a group is skipped for its holder
	for i, group := range groups {
		if group == nil {
			continue
		}
		holder := holders[*group]
		if holder == i {
			continue
		}
		skips[i] = &SkippedVariant{Index: holder, For: specs[holder].Resolution}
	}
	return skips
}

// Build and store the manifest from the fan-out's durable outputs
func (w *jobWorkflowHandlers) writeManifest(ctx context.Context, task francisworkflow.Task) (any, error) {
	req, namespace, imageID, err := decodeRequest(task)
	if err != nil {
		return nil, err
	}
	var snap jobSnapshot
	err = task.DecodeOutput(stepSnapshot, &snap)
	if err != nil {
		return nil, errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("failed to decode the snapshot: %w", err))
	}

	// A job with no variants only records the image, which the upload already did, so the manifest it found is the one it reports
	// An image whose manifest went missing gets one again, so it's never stored without being recorded
	if len(req.Variants) == 0 {
		previous := w.parsePrevious(ctx, snap.Previous, namespace, imageID)
		if previous != nil {
			return *previous, nil
		}
		stored := storedManifest(imagekeys.New(), req, snap, namespace, imageID)
		err = w.runtime.Store.WriteManifest(ctx, namespace, imageID, stored)
		if err != nil {
			return nil, fmt.Errorf("failed to write the manifest: %w", err)
		}
		return stored, nil
	}

	var tasks []variantTask
	err = task.DecodeOutput(stepPrepare, &tasks)
	if err != nil {
		return nil, errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("failed to decode the prepared variants: %w", err))
	}
	var results []VariantResult
	err = task.DecodeOutput(stepGenerate, &results)
	if err != nil {
		return nil, errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("failed to decode the variant results: %w", err))
	}
	if len(tasks) != len(results) || len(tasks) != len(req.Variants) {
		return nil, errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("variant result count %d does not match the prepared count %d", len(results), len(tasks)))
	}

	// The fan-out is fail-fast, so every durable output belongs to a variant that was stored successfully
	for i := range results {
		results[i].Index = i
		results[i].Name = tasks[i].Spec.Name
		if !results[i].Succeeded() {
			return nil, errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("variant result %d did not report success", i))
		}
	}

	m, err := buildManifest(imagekeys.New(), req, snap, tasks[0].SourceSize, results)
	if err != nil {
		return nil, errors.Join(actor.ErrJobPermanentFailure, err)
	}
	err = w.runtime.Store.WriteManifest(ctx, namespace, imageID, m)
	if err != nil {
		return nil, fmt.Errorf("failed to write the manifest: %w", err)
	}

	w.runtime.Log.InfoContext(ctx, "Wrote the manifest", slog.String("jobId", req.JobID))
	return m, nil
}

// Put back the manifest the job found, when Francis unwinds a job whose manifest step ran or failed partway
//
// The manifest a job found describes files that are still there, because the generation compensation keeps every file it references, so restoring it is what leaves the image as it was
func (w *jobWorkflowHandlers) restoreManifest(ctx context.Context, compensation francisworkflow.Compensation) error {
	req, namespace, imageID, err := decodeRequest(compensation)
	if err != nil {
		return err
	}
	// A job with no variants wrote nothing over the manifest it found
	if len(req.Variants) == 0 {
		return nil
	}

	var snap jobSnapshot
	err = compensation.DecodeOutput(stepSnapshot, &snap)
	if err != nil {
		return fmt.Errorf("failed to decode the snapshot for manifest compensation: %w", err)
	}

	if len(snap.Previous) > 0 {
		err = w.runtime.Store.WriteManifestRaw(ctx, namespace, imageID, snap.Previous)
	} else {
		err = w.runtime.Store.RemoveManifest(ctx, namespace, imageID)
	}
	if err != nil {
		return fmt.Errorf("failed to compensate the manifest: %w", err)
	}

	w.runtime.Log.InfoContext(ctx, "Put back the manifest the job found while compensating", slog.String("jobId", req.JobID))
	return nil
}

// Report the job's outcome to the endpoint the request named
func (w *jobWorkflowHandlers) notify(ctx context.Context, task francisworkflow.Task) (any, error) {
	req, _, _, err := decodeRequest(task)
	if err != nil {
		return nil, err
	}
	if req.Notify == nil {
		return struct{}{}, nil
	}

	var m manifest.Manifest
	err = task.DecodeOutput(stepManifest, &m)
	if err != nil {
		return nil, errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("failed to decode the manifest: %w", err))
	}

	eventType := webhook.EventImageOptimized
	if len(req.Variants) == 0 {
		eventType = webhook.EventImageStored
	}

	// The event's timestamp is what happened, not when this attempt is made, so it stays the same across retries
	timestamp := m.CompletedAt
	if timestamp.IsZero() {
		timestamp = m.StoredAt
	}
	if timestamp.IsZero() {
		timestamp = req.RequestedAt
	}

	err = w.deliver(ctx, req, webhook.Event{
		Type:      eventType,
		Timestamp: timestamp,
		Data:      w.eventData(req, &m),
	})
	if err != nil {
		return nil, err
	}

	w.runtime.Log.InfoContext(ctx, "Delivered the job's event", slog.String("jobId", req.JobID), slog.String("type", string(eventType)))
	return struct{}{}, nil
}

// Deliver one event to the endpoint the job resolved when it was queued
func (w *jobWorkflowHandlers) deliver(ctx context.Context, req JobRequest, event webhook.Event) error {
	if w.runtime.Notifier == nil {
		return errors.Join(actor.ErrJobPermanentFailure, errors.New("no webhook endpoints are configured"))
	}

	// The delivery ID is derived rather than random, so every attempt carries the same one and the receiver can tell a retry from a new event
	err := w.runtime.Notifier.Deliver(ctx, req.Notify.Endpoint, req.JobID+"."+string(event.Type), event)
	if errors.Is(err, notify.ErrPermanent) {
		return errors.Join(actor.ErrJobPermanentFailure, err)
	} else if err != nil {
		return fmt.Errorf("failed to deliver the '%s' event: %w", event.Type, err)
	}
	return nil
}

// Describe the job in an event
func (w *jobWorkflowHandlers) eventData(req JobRequest, m *manifest.Manifest) webhook.EventData {
	data := webhook.EventData{
		Namespace: req.Namespace,
		ImageID:   req.ImageID,
		JobID:     req.JobID,
		Manifest:  m,
	}
	imageID, err := uuid.Parse(req.ImageID)
	if err == nil {
		data.ManifestKey = w.runtime.Store.ManifestKey(req.Namespace, imageID)
	}
	if req.Notify != nil {
		data.Context = req.Notify.Context
	}
	return data
}

// Clean up the local source after a completed job and record its outcome
// Nothing here can fail the job, because its variants and manifest are already stored
func (w *jobWorkflowHandlers) cleanupCompleted(ctx context.Context, task francisworkflow.Task) (any, error) {
	req, namespace, imageID, err := decodeRequest(task)
	if err != nil {
		w.runtime.Log.WarnContext(ctx, "Failed to decode the job for cleanup", slog.Any("error", err))
		return struct{}{}, nil
	}

	w.removeSource(ctx, req.JobID, namespace, imageID)
	if w.runtime.Metrics != nil {
		w.runtime.Metrics.RecordJobCompleted(ctx, namespace, string(StatusCompleted))
	}
	w.runtime.Log.InfoContext(ctx, "Job completed",
		slog.String("jobId", req.JobID),
		slog.Int("variants", len(req.Variants)),
	)
	return struct{}{}, nil
}

// Tell the image's queue that this job is done, so the next one starts
func (w *jobWorkflowHandlers) release(ctx context.Context, task francisworkflow.Task) (any, error) {
	req, _, _, err := decodeRequest(task)
	if err != nil {
		return nil, err
	}

	err = w.releaseQueue(ctx, req)
	if err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

// Invoke the image actor's finished method, which ignores a job that isn't the active one, so a repeat is harmless
func (w *jobWorkflowHandlers) releaseQueue(ctx context.Context, req JobRequest) error {
	if w.runtime.Service == nil {
		return nil
	}

	_, err := w.runtime.Service.Invoke(ctx, imageActorType, imageActorID(req.Namespace, req.ImageID), methodFinished, finishedRequest{JobID: req.JobID})
	if err != nil {
		return fmt.Errorf("failed to release the image's queue: %w", err)
	}
	return nil
}

// Clean up when a job fails: free the queue, report the failure, and remove the local source
//
// It is the compensation of the first step, so it runs last, once every other effect of the job has been undone and the manifest put back
func (w *jobWorkflowHandlers) cleanupFailed(ctx context.Context, compensation francisworkflow.Compensation) error {
	req, namespace, imageID, err := decodeRequest(compensation)
	if err != nil {
		return err
	}

	// The queue is freed first, so a failure event that can't be delivered doesn't hold up the image's next job
	err = w.releaseQueue(ctx, req)
	if err != nil {
		w.runtime.Log.WarnContext(ctx, "Failed to release the queue of a failed job; the image actor will notice on its own", slog.String("jobId", req.JobID), slog.Any("error", err))
	}

	w.removeSource(ctx, req.JobID, namespace, imageID)
	cause := failureMessage(compensation.Cause())

	// The failure event carries the manifest the image is left with, which is the one the job found
	if req.Notify != nil {
		var previous *manifest.Manifest
		var snap jobSnapshot
		snapErr := compensation.DecodeResult(&snap)
		if snapErr == nil {
			previous = w.parsePrevious(ctx, snap.Previous, namespace, imageID)
		}

		event := webhook.Event{Type: webhook.EventImageFailed, Timestamp: req.RequestedAt, Data: w.eventData(req, previous)}
		event.Data.Error = cause
		err = w.deliver(ctx, req, event)
		if err != nil {
			// The delivery is retried a few times, and then given up on so the job can finish unwinding
			if compensation.Attempt() <= failureDeliveryAttempts && !errors.Is(err, actor.ErrJobPermanentFailure) {
				return fmt.Errorf("failed to deliver the failure event: %w", err)
			}
			w.runtime.Log.ErrorContext(ctx, "Gave up delivering the failure event of a job", slog.String("jobId", req.JobID), slog.Any("error", err))
		}
	}

	if w.runtime.Metrics != nil {
		w.runtime.Metrics.RecordJobCompleted(ctx, namespace, string(StatusFailed))
	}
	w.runtime.Log.InfoContext(ctx, "Job failed",
		slog.String("jobId", req.JobID),
		slog.String("error", cause),
	)
	return nil
}

// Remove this host's cached source without making local cleanup part of the durable result
func (w *jobWorkflowHandlers) removeSource(ctx context.Context, jobID string, namespace string, imageID uuid.UUID) {
	err := w.runtime.SourceCache.Remove(namespace, imageID)
	if err != nil {
		w.runtime.Log.WarnContext(ctx, "Failed to remove the locally cached source image", slog.String("jobId", jobID), slog.Any("error", err))
	}
}

// Read and check the job's input, which every step needs before it touches an object key
func decodeRequest(task francisworkflow.Task) (JobRequest, string, uuid.UUID, error) {
	var req JobRequest
	err := task.DecodeInput(&req)
	if err != nil {
		return JobRequest{}, "", uuid.Nil(), errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("failed to decode the job: %w", err))
	}

	err = imagekeys.ValidateNamespace(req.Namespace)
	if err != nil {
		return JobRequest{}, "", uuid.Nil(), errors.Join(actor.ErrJobPermanentFailure, err)
	}
	imageID, err := imagekeys.ValidateID(req.ImageID)
	if err != nil {
		return JobRequest{}, "", uuid.Nil(), errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("invalid image ID: %w", err))
	}

	return req, req.Namespace, imageID, nil
}

// Turn the cause Francis records for a failed job into the message a caller sees
// A missing original is reported by its code, so a receiver can match it without parsing the engine's wording around it
func failureMessage(cause string) string {
	idx := strings.Index(cause, ErrorImageNotFound)
	if idx >= 0 {
		return cause[idx:]
	}
	return cause
}

// Build the manifest of an image whose original is stored and that has no variants yet
func storedManifest(layout imagekeys.Layout, req JobRequest, snap jobSnapshot, namespace string, imageID uuid.UUID) manifest.Manifest {
	return manifest.Manifest{
		Version:      manifest.Version,
		Namespace:    namespace,
		ImageID:      imageID.String(),
		Status:       manifest.StatusStored,
		SourceKey:    layout.OriginalKey(namespace, imageID, snap.SourceFormat),
		SourceName:   snap.SourceName,
		SourceFormat: snap.SourceFormat,
		SourceSize:   snap.SourceSize,
		SourceWidth:  snap.SourceResolution.Width,
		SourceHeight: snap.SourceResolution.Height,
		StoredAt:     snap.StoredAt,
		RequestedAt:  req.RequestedAt,
		Variants:     []manifest.Variant{},
	}
}

// Build the manifest a completed job stores, which the webhook also carries
func buildManifest(layout imagekeys.Layout, req JobRequest, snap jobSnapshot, source imaging.Resolution, results []VariantResult) (manifest.Manifest, error) {
	if len(req.Variants) != len(results) {
		return manifest.Manifest{}, fmt.Errorf("variant result count %d does not match the requested count %d", len(results), len(req.Variants))
	}
	imageID, err := uuid.Parse(req.ImageID)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("invalid image ID: %w", err)
	}

	m := storedManifest(layout, req, snap, req.Namespace, imageID)
	m.Status = manifest.StatusCompleted
	m.JobID = req.JobID
	m.SourceWidth = source.Width
	m.SourceHeight = source.Height
	m.StartedAt = snap.StartedAt
	m.CompletedAt = snap.StartedAt
	m.Variants = make([]manifest.Variant, len(req.Variants))

	key := func(width int, height int, format imaging.Format) string {
		return layout.AssetKey(req.Namespace, imageID, width, height, format.Extension())
	}

	// Describe every variant that owns a file first, because a skipped one repeats the file of the variant that holds its pixels
	for i, spec := range req.Variants {
		result := results[i] //nolint:gosec // Result and request counts are required to match above
		if result.CompletedAt.After(m.CompletedAt) {
			m.CompletedAt = result.CompletedAt
		}
		if !result.Succeeded() {
			m.Status = manifest.StatusFailed
		}
		switch {
		case result.Skipped != nil:
			continue
		case result.Reused != nil:
			m.Variants[i] = reusedEntry(spec, *result.Reused)
		default:
			m.Variants[i] = manifest.Variant{
				Name:       spec.Name,
				Format:     spec.Format,
				Quality:    spec.Quality,
				Effort:     spec.Effort,
				Resolution: spec.Resolution,
				Fit:        spec.Fit,
				Width:      result.Width,
				Height:     result.Height,
				Size:       result.Size,
				Succeeded:  result.Succeeded(),
				Error:      result.Error,
			}
		}
		m.Variants[i].Key = key(m.Variants[i].Width, m.Variants[i].Height, m.Variants[i].Format)
	}

	// A skipped entry repeats its holder's file, so a reader that doesn't know about skipped entries sees a duplicate of that file
	// It's satisfied rather than failed, because the holder's file is what this box asked for
	for i, spec := range req.Variants {
		skip := results[i].Skipped //nolint:gosec // Result and request counts are required to match above
		if skip == nil {
			continue
		}
		if skip.Index < 0 || skip.Index >= len(results) || results[skip.Index].Skipped != nil {
			return manifest.Manifest{}, fmt.Errorf("variant %d is skipped for variant %d, which doesn't hold a file", i, skip.Index)
		}

		holder := m.Variants[skip.Index]
		skippedFor := skip.For
		m.Variants[i] = manifest.Variant{
			Key:        holder.Key,
			Name:       spec.Name,
			Format:     spec.Format,
			Quality:    holder.Quality,
			Effort:     holder.Effort,
			Resolution: spec.Resolution,
			Fit:        spec.Fit,
			Width:      holder.Width,
			Height:     holder.Height,
			Size:       holder.Size,
			Succeeded:  results[i].Succeeded() && holder.Succeeded, //nolint:gosec // Result and request counts are required to match above
			Skipped:    true,
			SkippedFor: &skippedFor,
		}
	}

	return m, nil
}

// Describe a reused file with the metadata of the encoding that stored it, under the name and box this job asked for
// The box can differ from the stored one when a box the source can't fill reuses the full-resolution file another box stored
func reusedEntry(spec VariantSpec, stored manifest.Variant) manifest.Variant {
	return manifest.Variant{
		Name:       spec.Name,
		Format:     stored.Format,
		Quality:    stored.Quality,
		Effort:     stored.Effort,
		Resolution: spec.Resolution,
		Fit:        spec.Fit,
		Width:      stored.Width,
		Height:     stored.Height,
		Size:       stored.Size,
		Succeeded:  stored.Succeeded,
		Error:      stored.Error,
	}
}
