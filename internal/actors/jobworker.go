package actors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	result, err := w.generate(ctx, input)
	if err == nil {
		return result, nil
	}
	if !isRetryable(err) {
		return nil, errors.Join(actor.ErrJobPermanentFailure, err)
	}

	return nil, err
}

// Generate the variant and return the result recorded by the workflow
func (w *jobWorkflowHandlers) generate(ctx context.Context, task variantTask) (VariantResult, error) {
	log := w.runtime.Log.With(
		slog.String("imageId", task.ImageID),
		slog.Int("index", task.Spec.Index),
		slog.String("name", task.Spec.Name),
	)

	result := VariantResult{
		Index: task.Spec.Index,
		Name:  task.Spec.Name,
		Done:  true,
	}
	// A box the source can't fill, whose pixels another variant of this job holds, encodes nothing: the manifest repeats the holder's file for it
	if task.Skip != nil {
		skip := *task.Skip
		result.CompletedAt = time.Now()
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
		result.CompletedAt = time.Now()
		result.Reused = &existing
		log.DebugContext(ctx, "Reused an existing variant",
			slog.Int("width", existing.Width),
			slog.Int("height", existing.Height),
			slog.Int64("size", existing.Size),
		)
		return result, nil
	}

	start := time.Now()
	res, err := w.encode(ctx, task)
	duration := time.Since(start)

	result.DurationMs = duration.Milliseconds()
	result.CompletedAt = time.Now()

	if err != nil {
		result.Error = err.Error()
		if w.runtime.Metrics != nil {
			w.runtime.Metrics.RecordVariant(ctx, string(task.Spec.Format), false, duration)
		}
		log.ErrorContext(ctx, "Failed to generate variant", slog.Any("error", err))
		return result, fmt.Errorf("failed to generate variant '%s': %w", task.Spec.Name, err)
	}

	result.Width = res.Width
	result.Height = res.Height
	result.Size = res.Size

	if w.runtime.Metrics != nil {
		w.runtime.Metrics.RecordVariant(ctx, string(task.Spec.Format), true, duration)
	}
	log.InfoContext(ctx, "Generated variant",
		slog.Int("width", res.Width),
		slog.Int("height", res.Height),
		slog.Int64("size", res.Size),
		slog.Duration("duration", duration),
	)

	return result, nil
}

// Read the source image, encode the variant, and write it back to the store
func (w *jobWorkflowHandlers) encode(ctx context.Context, task variantTask) (imaging.Result, error) {
	// Durable workflow input is re-checked where it becomes an object key
	namespace, imageID, err := taskIDs(task)
	if err != nil {
		return imaging.Result{}, fmt.Errorf("%w: %w", imaging.ErrInvalidRequest, err)
	}

	// A fan-out task may run on another host than prepare, so a local cache miss downloads the source on demand
	source, err := w.runtime.SourceCache.Read(ctx, namespace, imageID, task.SourceFormat)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrTooLarge) {
			return imaging.Result{}, fmt.Errorf("%w: %w", imaging.ErrSourceUnreadable, err)
		}
		return imaging.Result{}, fmt.Errorf("failed to read the source image: %w", err)
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
		return imaging.Result{}, fmt.Errorf("failed to encode the variant: %w", err)
	}

	// A retried workflow task writes the same key, so the last complete encode wins without leaving duplicates
	err = w.runtime.Store.SaveVariant(ctx, namespace, imageID, res.Width, res.Height, task.Spec.Format.Extension(), res.Data, task.Spec.Format.ContentType())
	if err != nil {
		return imaging.Result{}, fmt.Errorf("failed to store the variant: %w", err)
	}

	return res, nil
}

// Delete one variant when a later task fails and Francis unwinds the fan-out
//
// A file the manifest the job found still lists is kept, because a variant of the same size and format is stored under the same key, and that manifest is the one the image is left with
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
	// Reused and skipped variants stored nothing in this attempt, so there's nothing of theirs to delete
	if result.Reused != nil || result.Skipped != nil {
		return nil
	}

	namespace, imageID, err := taskIDs(input)
	if err != nil {
		return errors.Join(actor.ErrJobPermanentFailure, err)
	}

	// Without the snapshot it isn't known what the previous manifest references, and keeping an orphaned file is the harmless mistake
	var snap jobSnapshot
	err = compensation.DecodeOutput(stepSnapshot, &snap)
	if err != nil {
		w.runtime.Log.WarnContext(ctx, "Kept a generated variant while compensating, because the job's snapshot couldn't be read", slog.String("name", input.Spec.Name), slog.Any("error", err))
		return nil
	}
	previous := w.parsePrevious(ctx, snap.Previous, namespace, imageID)
	if previous != nil {
		for _, entry := range previous.Variants {
			if !entry.Skipped && entry.Width == result.Width && entry.Height == result.Height && entry.Format.Extension() == input.Spec.Format.Extension() {
				return nil
			}
		}
	}

	err = w.runtime.Store.RemoveVariant(ctx, namespace, imageID, result.Width, result.Height, input.Spec.Format.Extension())
	if err != nil {
		return fmt.Errorf("failed to compensate the generated variant: %w", err)
	}

	w.runtime.Log.InfoContext(ctx, "Removed a generated variant while compensating the job",
		slog.String("name", input.Spec.Name),
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
