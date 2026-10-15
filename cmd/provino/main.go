package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	configkit "github.com/italypaleale/go-kit/config"
	"github.com/italypaleale/go-kit/observability"
	"github.com/italypaleale/go-kit/servicerunner"
	"github.com/italypaleale/go-kit/signals"
	slogkit "github.com/italypaleale/go-kit/slog"

	"github.com/italypaleale/provino/internal/actors"
	"github.com/italypaleale/provino/internal/auth"
	"github.com/italypaleale/provino/internal/authorizer"
	"github.com/italypaleale/provino/internal/buildinfo"
	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/internal/imaging"
	appmetrics "github.com/italypaleale/provino/internal/metrics"
	"github.com/italypaleale/provino/internal/notify"
	"github.com/italypaleale/provino/internal/server"
	"github.com/italypaleale/provino/internal/signing"
	"github.com/italypaleale/provino/internal/store"
)

// @title provino API
// @version 0.1.0
// @description HTTP API for uploading images and generating their variants in durable jobs
// @BasePath /
// @securityDefinitions.apikey Bearer
// @in header
// @name Authorization
// @description `Bearer <token>`, where the token is a JWT from the configured issuer or an API key starting with `pvk_`
func main() {
	// Init a logger used for initialization only, to report initialization errors
	initLogger := slog.Default().
		With(slog.String("app", buildinfo.AppName)).
		With(slog.String("version", buildinfo.AppVersion))

	// Load config
	cfg := config.Get()
	err := configkit.LoadConfig(cfg, configkit.LoadConfigOpts{
		EnvVar:  "PROVINO_CONFIG",
		DirName: "provino",
	})
	if err != nil {
		ce, ok := errors.AsType[*configkit.ConfigError](err)
		if ok {
			ce.LogFatal(initLogger)
		} else {
			slogkit.FatalError(initLogger, "Failed to load configuration", err)
			return
		}
	}

	// List of services to run
	services := make([]servicerunner.Service, 0, 3)
	shutdowns := &shutdownManager{
		fns: make([]servicerunner.Service, 0, 4),
	}

	// Get a context that is canceled when the application receives a termination signal.
	ctx := signals.SignalContext(context.Background())

	// Get the logger
	log, loggerShutdownFn, err := observability.InitLogs(ctx, observability.InitLogsOpts{
		Config:     cfg,
		Level:      cfg.Logs.Level,
		JSON:       cfg.Logs.JSON,
		AppName:    buildinfo.AppName,
		AppVersion: buildinfo.AppVersion,
	})
	if err != nil {
		slogkit.FatalError(initLogger, "Failed to create logger", err)
		return
	}
	slog.SetDefault(log)
	shutdowns.Add(loggerShutdownFn)

	// Validate the configuration
	err = cfg.Validate(log)
	if err != nil {
		shutdowns.Run(log)
		slogkit.FatalError(log, "Invalid configuration", err)
		return
	}

	log.Info("Starting provino", slog.String("build", buildinfo.BuildDescription))

	// Load the credentials callers authenticate with, which fetches the JWT key set before any request is accepted
	authenticators, err := auth.NewSet(ctx, cfg.Auth, cfg.Server.PublicURL)
	if err != nil {
		shutdowns.Run(log)
		slogkit.FatalError(log, "Failed to initialize authentication", err)
		return
	}
	shutdowns.Add(authenticators.Close)

	// Init appMetrics
	appMetrics, metricsShutdownFn, err := appmetrics.NewAppMetrics(ctx)
	if err != nil {
		shutdowns.Run(log)
		slogkit.FatalError(log, "Failed to init metrics", err)
		return
	}
	shutdowns.Add(metricsShutdownFn)

	// Init tracing
	traceProvider, tracerShutdownFn, err := observability.InitTraces(ctx, observability.InitTracesOpts{
		Config:  cfg,
		AppName: buildinfo.AppName,
	})
	if err != nil {
		shutdowns.Run(log)
		slogkit.FatalError(log, "Failed to init tracing", err)
		return
	}
	shutdowns.Add(tracerShutdownFn)

	// Init libvips, which generates the variants
	err = imaging.Startup(ctx, imaging.Options{
		CacheMaxMemory: cfg.Images.VipsCacheMaxMemory,
		Logger:         log.With(slog.String("scope", "libvips")),
	})
	if err != nil {
		shutdowns.Run(log)
		slogkit.FatalError(log, "Failed to init libvips", err)
		return
	}
	shutdowns.Add(func(context.Context) error {
		imaging.Shutdown()
		return nil
	})

	encoder := imaging.NewEncoder()

	// Init the store where the source images and the variants are kept
	imageStore, err := store.New(store.NewOpts{
		OriginalsDSN: cfg.Storage.Originals,
		VariantsDSN:  cfg.Storage.Variants,
	})
	if err != nil {
		shutdowns.Run(log)
		slogkit.FatalError(log, "Failed to init the image store", err)
		return
	}
	log.Info("Storage initialized",
		slog.String("originals", imageStore.OriginalsDescription()),
		slog.String("variants", imageStore.VariantsDescription()),
	)

	// The signing keys, the webhook endpoints, and the authorizer are all set up before the host starts, so a misconfigured one fails startup rather than the first job or upload
	signingKeys, err := signing.LoadKeySet(cfg.Signing.Ed25519)
	if err != nil {
		shutdowns.Run(log)
		slogkit.FatalError(log, "Failed to load the signing keys", err)
		return
	}
	notifier, err := notify.New(cfg.Notify, signingKeys)
	if err != nil {
		shutdowns.Run(log)
		slogkit.FatalError(log, "Failed to initialize the webhook endpoints", err)
		return
	}
	uploadAuthorizer, err := authorizer.New(cfg.Authorizer, cfg.Images, signingKeys)
	if err != nil {
		shutdowns.Run(log)
		slogkit.FatalError(log, "Failed to initialize the authorizer", err)
		return
	}
	if uploadAuthorizer == nil {
		log.Info("Browser uploads are disabled because no authorizer is configured")
	}

	// Create the actor host, which runs the durable jobs
	log.Info("Initializing actor host", slog.String("runtime", cfg.Actors.Runtime))
	actorHost, err := actors.NewHost(actors.NewHostOpts{
		Store:      imageStore,
		Encoder:    encoder,
		AppMetrics: appMetrics,
		Logger:     log,
		Notifier:   notifier,
	})
	if err != nil {
		shutdowns.Run(log)
		slogkit.FatalError(log, "Failed to init actor host", err)
		return
	}
	services = append(services, actorHost.Run)

	// Create HTTP server
	log.Info("Initializing API server")
	apiServer, err := server.NewServer(server.NewServerOpts{
		AppMetrics:    appMetrics,
		TraceProvider: traceProvider,
		Jobs:          actorHost.Jobs(),
		RateLimiter:   actorHost.RateLimiter(),
		Store:         imageStore,
		SourceCache:   actorHost.SourceCache(),
		Measurer:      encoder,
		Authenticator: authenticators,
		Authorizer:    uploadAuthorizer,
		Notifier:      notifier,
		SigningKeys:   signingKeys,
		Ready:         actorHost.Ready(),
	})
	if err != nil {
		shutdowns.Run(log)
		slogkit.FatalError(log, "Failed to init API server", err)
		return
	}
	services = append(services, apiServer.Run)

	// Run all services
	// This call blocks until the context is canceled
	err = servicerunner.
		NewServiceRunner(services...).
		Run(ctx)
	if err != nil {
		shutdowns.Run(log)
		slogkit.FatalError(log, "Failed to run service", err)
		return
	}

	// Run all shutdown sequences
	shutdowns.Run(log)
}

type shutdownManager struct {
	fns []servicerunner.Service
}

func (s *shutdownManager) Add(fn servicerunner.Service) {
	if fn == nil {
		return
	}
	s.fns = append(s.fns, fn)
}

func (s *shutdownManager) Run(log *slog.Logger) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	err := servicerunner.
		NewServiceRunner(s.fns...).
		Run(shutdownCtx)
	if err != nil {
		log.Error("Error shutting down services", slog.Any("error", err))
	}
}
