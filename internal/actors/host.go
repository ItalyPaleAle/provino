package actors

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/italypaleale/francis/actor"
	"github.com/italypaleale/francis/builtin/ratelimit"
	"github.com/italypaleale/francis/host"
	"github.com/italypaleale/francis/host/local"
	"github.com/italypaleale/francis/host/remote"
	"github.com/italypaleale/go-kit/utils"

	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/internal/imaging"
	"github.com/italypaleale/provino/internal/metrics"
	"github.com/italypaleale/provino/internal/notify"
	"github.com/italypaleale/provino/internal/sourcecache"
	"github.com/italypaleale/provino/internal/store"
)

// Runtime contains everything the actors need to do their work
// It's shared by every actor instance the host creates, since the actors themselves are short-lived
type Runtime struct {
	Store *store.Store
	// Local copy of the source image every variant of a job is encoded from, so the job downloads it once instead of once per variant
	SourceCache *sourcecache.Cache
	Encoder     *imaging.Encoder
	Metrics     *metrics.AppMetrics
	Log         *slog.Logger
	// Webhook endpoints jobs report their outcome to, or nil when none are configured
	Notifier *notify.Notifier
	// Actor service, which a job uses to tell its image's actor that it finished
	Service *actor.Service

	// How long each forward or compensation handler may execute after a worker starts it
	WorkflowTimeout time.Duration
	// How long the status of a finished job is retained; zero retains it indefinitely
	CompletedWorkflowRetention time.Duration
}

// Host runs the Francis actor host that executes the jobs
//
// The runtime backing it is chosen by "actors.runtime": embedded in this process (Francis' "local" topology), which needs no control plane to be deployed alongside the service, or a standalone Francis runtime this process only connects to (the "remote" topology), which is what scaling past a handful of instances wants
// Both hosts implement the same host.Host interface, so everything below this line is identical in either topology
type Host struct {
	host        host.Host
	jobs        *hostJobs
	rateLimiter *ratelimit.RateLimitService
	sourceCache *sourcecache.Cache
}

// NewHostOpts contains the options for NewHost
type NewHostOpts struct {
	Store      *store.Store
	Encoder    *imaging.Encoder
	AppMetrics *metrics.AppMetrics
	Logger     *slog.Logger
	Notifier   *notify.Notifier

	// Overrides used by the tests only, so a test doesn't run on the production intervals
	alarmsPollInterval time.Duration
}

// NewHost creates the actor host and registers the image actor and the job workflow
func NewHost(opts NewHostOpts) (*Host, error) {
	cfg := config.Get()

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// The cache is created before the runtime because every actor shares it, and a folder that can't be created is a failure worth reporting at startup rather than on the first job
	// Derive the cache age from the handler budget so abandoned files remain bounded while later workers can still download a swept source again
	sourceCache, err := sourcecache.New(sourcecache.Options{
		Source:  opts.Store,
		Dir:     cfg.Storage.SourceCache,
		MaxSize: cfg.Images.MaxUploadSize,
		MaxAge:  2 * cfg.Actors.WorkflowTimeout,
		Logger:  logger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create the source image cache: %w", err)
	}

	// The topology decides how the host is created, and nothing else
	var h host.Host
	switch cfg.Actors.Runtime {
	case config.ActorRuntimeExternal:
		h, err = newExternalHost(cfg, logger)
	default:
		h, err = newEmbeddedHost(cfg, logger, opts.alarmsPollInterval)
	}
	if err != nil {
		return nil, err
	}

	rt := &Runtime{
		Store:                      opts.Store,
		SourceCache:                sourceCache,
		Encoder:                    opts.Encoder,
		Metrics:                    opts.AppMetrics,
		Log:                        logger.With(slog.String("scope", "actors")),
		Notifier:                   opts.Notifier,
		Service:                    h.Service(),
		WorkflowTimeout:            cfg.Actors.WorkflowTimeout,
		CompletedWorkflowRetention: cfg.Actors.CompletedWorkflowRetention,
	}

	// Register the shared per-caller limiter before the host starts so every topology routes checks through Francis
	rateLimiterActor, err := ratelimit.New("provino-ingest",
		ratelimit.WithRate(cfg.RateLimit.Rate),
		ratelimit.WithPer(cfg.RateLimit.Per),
		ratelimit.WithBurst(cfg.RateLimit.Burst),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create the ingest rate limiter: %w", err)
	}
	err = h.RegisterBuiltInActor(rateLimiterActor)
	if err != nil {
		return nil, fmt.Errorf("failed to register the ingest rate limiter: %w", err)
	}

	// Francis owns placement and shares this strict per-host budget across every worker queue in the workflow
	// The cap is sized for encodes, which are the expensive step; the other steps draw from the same budget
	maxConcurrentEncodes := cfg.Images.MaxConcurrentEncodes
	if maxConcurrentEncodes == 0 {
		// GOMAXPROCS rather than NumCPU, because it follows a container's CPU limit while NumCPU counts every core of the machine
		maxConcurrentEncodes = runtime.GOMAXPROCS(0)
	}

	jobWorkflow, err := NewJobWorkflow(rt, maxConcurrentEncodes)
	if err != nil {
		return nil, err
	}

	err = h.RegisterBuiltInActor(jobWorkflow)
	if err != nil {
		return nil, fmt.Errorf("failed to register the job workflow '%s': %w", jobWorkflow.Name(), err)
	}

	jobs := &jobService{
		workflow: jobWorkflow.Service(h.Service()),
		runtime:  rt,
	}

	// The image actor is what serializes the jobs of one image, so it starts every workflow
	err = h.RegisterActor(imageActorType, newImageActorFactory(rt, jobs))
	if err != nil {
		return nil, fmt.Errorf("failed to register the image actor: %w", err)
	}

	return &Host{
		host: h,
		jobs: &hostJobs{
			service: h.Service(),
			jobs:    jobs,
		},
		rateLimiter: rateLimiterActor.Service(h.Service()),
		sourceCache: sourceCache,
	}, nil
}

// Jobs returns what the API handlers use to start jobs and read their status
func (h *Host) Jobs() Jobs {
	return h.jobs
}

// RateLimiter returns the Francis service used to protect browser uploads
func (h *Host) RateLimiter() *ratelimit.RateLimitService {
	return h.rateLimiter
}

// SourceCache returns the local cache of source images, which the upload handler writes into as an upload streams by
func (h *Host) SourceCache() *sourcecache.Cache {
	return h.sourceCache
}

// Ready returns a channel that is closed once the actor host has registered itself and can execute workflows
func (h *Host) Ready() <-chan struct{} {
	return h.host.Ready()
}

// Run the actor host
// Note this function is blocking, and will return only when the host is shut down via context cancellation.
func (h *Host) Run(ctx context.Context) error {
	// The janitor of the source image cache lives exactly as long as the host, because the files it reclaims are the ones this host's workflows left behind
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() {
		_ = h.sourceCache.Run(ctx)
	})

	err := h.host.Run(ctx)

	// Stopping the janitor is what the cancel above does
	// Waiting for it keeps it from outliving the call that started it
	cancel()
	wg.Wait()

	//nolint:wrapcheck
	return err
}

// Create the host that runs the actor runtime inside this process
// This process is both the actor host and its own control plane, which is why it's the one that needs a database
func newEmbeddedHost(cfg *config.Config, logger *slog.Logger, pollIntervalOverride time.Duration) (host.Host, error) {
	providerOpt, err := providerOption(cfg)
	if err != nil {
		return nil, err
	}

	pollInterval := utils.PositiveOr(pollIntervalOverride, 30*time.Second)

	// The runtime PSK derives the cluster CA the hosts use to authenticate each other with mTLS, which only matters once a second instance shares the same database
	h, err := local.NewHost(
		local.WithAddress(cfg.Actors.Address),
		local.WithBindAddress(cfg.Actors.BindAddress),
		local.WithBindPort(cfg.Actors.BindPort),
		local.WithRuntimePSKs(cfg.GetRuntimePSK()),
		local.WithLogger(logger.With(slog.String("scope", "actor-host"))),
		local.WithAlarmsPollInterval(pollInterval),
		local.WithAlarmsFetchAheadInterval(pollInterval),
		providerOpt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create the actor host: %w", err)
	}

	return h, nil
}

// Create the host that connects to a standalone Francis runtime
// The runtime owns placement, durable job and alarm scheduling, and the database, so this process only contributes the actors it registers
func newExternalHost(cfg *config.Config, logger *slog.Logger) (host.Host, error) {
	external := cfg.Actors.External

	opts := []remote.HostOption{
		remote.WithAddress(cfg.Actors.Address),
		remote.WithBindAddress(cfg.Actors.BindAddress),
		remote.WithBindPort(cfg.Actors.BindPort),
		remote.WithRuntimeAddresses(external.Addresses...),
		remote.WithLogger(logger.With(slog.String("scope", "actor-host"))),
	}

	// The runtime authenticates this host with exactly one of the two methods, and the configuration has already established that only one is set
	if external.BootstrapPSK != "" {
		opts = append(opts, remote.WithHostBootstrapPSK([]byte(external.BootstrapPSK)))
	} else {
		opts = append(opts, remote.WithHostBootstrapJWTFile(external.BootstrapTokenFile))
	}

	// Pinning the cluster CA is what closes the trust gap on the first connection, so opting out of it is deliberate
	caPEM := cfg.GetExternalCAPEM()
	if len(caPEM) > 0 {
		opts = append(opts, remote.WithPinnedCA(caPEM))
	} else {
		opts = append(opts, remote.WithUnsafeNoPinnedCA())
	}

	h, err := remote.NewHost(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create the actor host: %w", err)
	}

	return h, nil
}

// Build the host option for the database the embedded runtime persists actor state, jobs, and alarms to
func providerOption(cfg *config.Config) (local.HostOption, error) {
	database := cfg.GetDatabase()

	switch database.Kind {
	case config.DatabaseKindSQLite:
		// The database file is created by the provider, but the folder that contains it isn't
		err := createSQLiteFolder(database)
		if err != nil {
			return nil, err
		}

		return local.WithSQLiteProvider(local.SQLiteProviderOptions{
			ConnectionString: database.ConnectionString,
		}), nil
	case config.DatabaseKindPostgres:
		return local.WithPostgresProvider(local.PostgresProviderOptions{
			ConnectionString: database.ConnectionString,
		}), nil
	default:
		return nil, fmt.Errorf("unsupported database '%s' for the embedded actor runtime", database.Kind)
	}
}

// Create the folder that contains the SQLite database file
// Connection strings that aren't plain paths are left to the provider, which knows how to parse them
func createSQLiteFolder(database config.Database) error {
	if !database.IsFilePath() {
		return nil
	}

	dir := filepath.Dir(database.ConnectionString)
	if dir == "" || dir == "." {
		return nil
	}

	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		return fmt.Errorf("failed to create folder '%s' for the SQLite database: %w", dir, err)
	}

	return nil
}
