package actors

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"
	"uuid"

	"github.com/italypaleale/francis/actor"
	francisworkflow "github.com/italypaleale/francis/builtin/workflow"

	"github.com/italypaleale/provino/imagekeys"
	"github.com/italypaleale/provino/internal/imaging"
	"github.com/italypaleale/provino/internal/store"
)

// Generate one variant and return its result to the workflow journal
func (w *jobWorkflowHandlers) generateVariant(ctx context.Context, task francisworkflow.Task) (any, error) {
	var input variantTask
	err := task.DecodeItem(&input)
	if err != nil {
		return nil, errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("failed to decode the variant task: %w", err))
	}

	// Permanent failures stop the fan-out immediately while transient failures retain Francis' retry policy
	result, err := w.generate(ctx, task.Index(), input)
	if err == nil {
		return result, nil
	}
	if !isRetryable(err) {
		return nil, errors.Join(actor.ErrJobPermanentFailure, err)
	}

	return nil, err
}

// Generate the variant at a position of the fan-out and return the result recorded by the workflow
func (w *jobWorkflowHandlers) generate(ctx context.Context, index int, task variantTask) (VariantResult, error) {
	log := w.runtime.Log.With(
		slog.String("imageId", task.ImageID),
		slog.Int("index", index),
		slog.String("format", string(task.Spec.Format)),
		slog.String("box", task.Spec.Resolution.String()),
	)

	var result VariantResult
	// A box the source can't fill, whose pixels another variant of this job holds, encodes nothing: the manifest repeats the holder's file for it
	if task.Skip != nil {
		skip := *task.Skip
		result.CompletedAt = time.Now().UTC()
		result.Skipped = &skip
		log.DebugContext(ctx, "Skipped a variant the source can't fill",
			slog.Int("heldBy", skip.Index),
			slog.String("heldFor", skip.For.String()),
		)
		return result, nil
	}

	if task.Existing != nil {
		existing := *task.Existing
		result.Width = existing.Width
		result.Height = existing.Height
		result.Size = existing.Size
		result.CompletedAt = time.Now().UTC()
		result.Reused = &existing
		log.DebugContext(ctx, "Reused an existing variant",
			slog.Int("width", existing.Width),
			slog.Int("height", existing.Height),
			slog.Int64("size", existing.Size),
		)
		return result, nil
	}

	start := time.Now()
	res, key, err := w.encode(ctx, task)
	duration := time.Since(start)

	if err != nil {
		w.runtime.Metrics.RecordVariant(ctx, string(task.Spec.Format), false, duration)
		log.ErrorContext(ctx, "Failed to generate variant", slog.Any("error", err))
		return VariantResult{}, fmt.Errorf("failed to generate variant %d: %w", index, err)
	}

	result.Width = res.Width
	result.Height = res.Height
	result.Size = res.Size
	result.DurationMs = duration.Milliseconds()
	result.CompletedAt = time.Now().UTC()
	result.Key = key

	w.runtime.Metrics.RecordVariant(ctx, string(task.Spec.Format), true, duration)
	log.InfoContext(ctx, "Generated variant",
		slog.String("key", key),
		slog.Int("width", res.Width),
		slog.Int("height", res.Height),
		slog.Int64("size", res.Size),
		slog.Duration("duration", duration),
	)

	return result, nil
}

// Encode the variant from the locally cached source, and store it at a key named after the size it came out at and the task's file ID
// It returns the key, which the manifest lists and a failed job deletes
func (w *jobWorkflowHandlers) encode(ctx context.Context, task variantTask) (imaging.Result, string, error) {
	// Durable workflow input is re-checked where it becomes an object key
	namespace, imageID, err := taskIDs(task)
	if err != nil {
		return imaging.Result{}, "", fmt.Errorf("%w: %w", imaging.ErrInvalidRequest, err)
	}
	err = imagekeys.ValidateFileID(task.FileID)
	if err != nil {
		return imaging.Result{}, "", fmt.Errorf("%w: %w", imaging.ErrInvalidRequest, err)
	}

	// A fan-out task may run on another host than prepare, so a local cache miss downloads the source on demand
	source, err := w.runtime.SourceCache.Get(ctx, namespace, imageID, task.SourceFormat)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrTooLarge) {
			return imaging.Result{}, "", fmt.Errorf("%w: %w", imaging.ErrSourceUnreadable, err)
		}
		return imaging.Result{}, "", fmt.Errorf("failed to read the source image: %w", err)
	}

	res, err := w.runtime.Encoder.Generate(ctx, imaging.Request{
		Source:     source,
		Format:     task.Spec.Format,
		Quality:    task.Spec.Quality,
		Effort:     task.Spec.Effort,
		Resolution: task.Spec.Resolution,
		Fit:        task.Spec.Fit,
	})
	if err != nil {
		// libvips opens the cached file by its path, so a sweep that removed it in between looks like an unreadable source, when downloading it again is all it takes
		// The error is reported without wrapping, so it isn't classified as permanent
		if errors.Is(err, imaging.ErrSourceUnreadable) {
			_, statErr := os.Stat(source)
			if errors.Is(statErr, fs.ErrNotExist) {
				return imaging.Result{}, "", fmt.Errorf("the cached source image was removed before it could be encoded: %s", err.Error())
			}
		}
		return imaging.Result{}, "", fmt.Errorf("failed to encode the variant: %w", err)
	}

	// The file ID is one no file of the image had when the job was planned, so this never writes over a file a manifest lists
	// A retried task encodes the same size and has the same file ID, so it writes the same key instead of leaving another file behind
	key := w.runtime.Store.AssetKey(namespace, imageID, res.Width, res.Height, task.FileID, task.Spec.Format)
	err = w.runtime.Store.SaveVariant(ctx, key, res.Data, task.Spec.Format.ContentType())
	if err != nil {
		return imaging.Result{}, "", fmt.Errorf("failed to store the variant: %w", err)
	}

	return res, key, nil
}

// Delete the file one variant stored when a later task fails and Francis unwinds the fan-out
//
// Every file a job encodes is at a key it chose, which no file of the image had, so a failed job deletes all of them, while reused and skipped variants stored nothing
// A retried compensation finds the file already gone, which isn't an error
func (w *jobWorkflowHandlers) deleteVariant(ctx context.Context, compensation francisworkflow.Compensation) error {
	var input variantTask
	err := compensation.DecodeItem(&input)
	if err != nil {
		return errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("failed to decode the variant task for compensation: %w", err))
	}

	var result VariantResult
	err = compensation.DecodeResult(&result)
	if err != nil {
		return errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("failed to decode the variant result for compensation: %w", err))
	}
	if result.Key == "" {
		return nil
	}

	// The key comes from the journal, so it's checked to be one of the image's objects before anything is deleted
	namespace, imageID, err := taskIDs(input)
	if err != nil {
		return errors.Join(actor.ErrJobPermanentFailure, err)
	}
	if !w.runtime.Store.OwnsKey(namespace, imageID, result.Key) {
		return errors.Join(actor.ErrJobPermanentFailure, fmt.Errorf("the key '%s' recorded for variant %d isn't one of the image's objects", result.Key, compensation.Index()))
	}

	err = w.runtime.Store.RemoveVariant(ctx, result.Key)
	if err != nil {
		return fmt.Errorf("failed to compensate the generated variant: %w", err)
	}

	w.runtime.Log.InfoContext(ctx, "Removed a generated variant while compensating the job",
		slog.Int("index", compensation.Index()),
		slog.String("key", result.Key),
	)
	return nil
}

// Report whether a generation failure is worth another attempt
// A request the encoder rejects, or a source image it cannot read, fails the same way every time
func isRetryable(err error) bool {
	return !errors.Is(err, imaging.ErrInvalidRequest) && !errors.Is(err, imaging.ErrSourceUnreadable)
}

// Parse the identifiers a variant task carries before they become object keys
func taskIDs(task variantTask) (namespace string, imageID uuid.UUID, err error) {
	err = imagekeys.ValidateNamespace(task.Namespace)
	if err != nil {
		return "", uuid.Nil(), err
	}

	imageID, err = imagekeys.ValidateID(task.ImageID)
	if err != nil {
		return "", uuid.Nil(), fmt.Errorf("invalid image ID: %w", err)
	}

	return task.Namespace, imageID, nil
}
