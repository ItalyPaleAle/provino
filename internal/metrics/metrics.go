package metrics

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	api "go.opentelemetry.io/otel/metric"

	"github.com/italypaleale/go-kit/observability"

	"github.com/italypaleale/provino/internal/buildinfo"
	"github.com/italypaleale/provino/internal/config"
)

const prefix = "provino"

type AppMetrics struct {
	apiCall          api.Int64Counter
	jobStarted       api.Int64Counter
	jobCompleted     api.Int64Counter
	variantGenerated api.Int64Counter
	variantDuration  api.Float64Histogram
}

func NewAppMetrics(ctx context.Context) (m *AppMetrics, shutdownFn func(ctx context.Context) error, err error) {
	cfg := config.Get()

	m = &AppMetrics{}

	meter, shutdownFn, err := observability.InitMetrics(ctx, observability.InitMetricsOpts{
		Config:  cfg,
		AppName: buildinfo.AppName,
		Prefix:  prefix,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to init metrics: %w", err)
	}

	m.apiCall, err = meter.Int64Counter(
		prefix+"_api_calls",
		api.WithDescription("The number of API calls"),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create "+prefix+"_api_calls meter: %w", err)
	}

	m.jobStarted, err = meter.Int64Counter(
		prefix+"_jobs_started",
		api.WithDescription("The number of jobs whose workflow was started"),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create "+prefix+"_jobs_started meter: %w", err)
	}

	m.jobCompleted, err = meter.Int64Counter(
		prefix+"_jobs_completed",
		api.WithDescription("The number of jobs that reached a terminal state"),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create "+prefix+"_jobs_completed meter: %w", err)
	}

	m.variantGenerated, err = meter.Int64Counter(
		prefix+"_variants_generated",
		api.WithDescription("The number of variants whose generation was attempted"),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create "+prefix+"_variants_generated meter: %w", err)
	}

	m.variantDuration, err = meter.Float64Histogram(
		prefix+"_variant_duration_seconds",
		api.WithDescription("The time it took to generate a variant"),
		api.WithUnit("s"),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create "+prefix+"_variant_duration_seconds meter: %w", err)
	}

	return m, shutdownFn, nil
}

// RecordAPICall records an invocation of an API endpoint
func (m *AppMetrics) RecordAPICall(ctx context.Context, method string) {
	if m == nil {
		return
	}

	m.apiCall.Add(recordCtx(ctx), 1,
		api.WithAttributeSet(attribute.NewSet(
			attribute.String("method", method),
		)),
	)
}

// RecordJobStarted records a job whose workflow began executing, along with the namespace it belongs to and the number of variants it generates
func (m *AppMetrics) RecordJobStarted(ctx context.Context, namespace string, variants int) {
	if m == nil {
		return
	}

	m.jobStarted.Add(recordCtx(ctx), 1,
		api.WithAttributeSet(attribute.NewSet(
			attribute.String("namespace", namespace),
			attribute.Int("variants", variants),
		)),
	)
}

// RecordJobCompleted records a job that reached a terminal state
func (m *AppMetrics) RecordJobCompleted(ctx context.Context, namespace string, status string) {
	if m == nil {
		return
	}

	m.jobCompleted.Add(recordCtx(ctx), 1,
		api.WithAttributeSet(attribute.NewSet(
			attribute.String("namespace", namespace),
			attribute.String("status", status),
		)),
	)
}

// RecordVariant records the outcome and duration of a single variant generation
func (m *AppMetrics) RecordVariant(ctx context.Context, format string, success bool, duration time.Duration) {
	if m == nil {
		return
	}

	attrs := api.WithAttributeSet(attribute.NewSet(
		attribute.String("format", format),
		attribute.Bool("success", success),
	))

	recordContext := recordCtx(ctx)
	m.variantGenerated.Add(recordContext, 1, attrs)
	m.variantDuration.Record(recordContext, duration.Seconds(), attrs)
}

// Returns a context that is safe to record metrics with, since recording must not be interrupted by the caller's cancellation
//
//nolint:contextcheck
func recordCtx(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}
