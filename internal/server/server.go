package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	httpserver "github.com/italypaleale/go-kit/httpserver"
	tlsconfig "github.com/italypaleale/go-kit/httpserver/tlsconfig"
	sloghttp "github.com/samber/slog-http"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/sdk/trace"

	provinoopenapi "github.com/italypaleale/provino/api/openapi"
	"github.com/italypaleale/provino/internal/actors"
	"github.com/italypaleale/provino/internal/auth"
	"github.com/italypaleale/provino/internal/authorizer"
	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/internal/imaging"
	"github.com/italypaleale/provino/internal/metrics"
	"github.com/italypaleale/provino/internal/notify"
	"github.com/italypaleale/provino/internal/signing"
	"github.com/italypaleale/provino/internal/sourcecache"
	"github.com/italypaleale/provino/internal/store"
)

// SourceMeasurer reads the size of an image the way its variants see it, after its EXIF orientation
type SourceMeasurer interface {
	SourceSize(ctx context.Context, source []byte) (imaging.Resolution, error)
}

// Server is the API server
type Server struct {
	appSrv  *http.Server
	handler http.Handler
	running atomic.Bool
	wg      sync.WaitGroup

	appMetrics    *metrics.AppMetrics
	traceProvider *trace.TracerProvider
	jobs          actors.Jobs
	ingestLimiter RateLimiter
	statusLimiter RateLimiter
	store         *store.Store
	sourceCache   *sourcecache.Cache
	measurer      SourceMeasurer
	authenticator auth.Authenticator
	authorizer    authorizer.Authorizer
	notifier      *notify.Notifier
	jwks          []byte
	ready         <-chan struct{}
	// Reverse proxies whose forwarding headers name the client of a request
	trustedProxies []netip.Prefix

	// Method that forces a reload of TLS certificates from disk
	tlsCertWatchFn tlsconfig.CertWatchFn

	// TLS configuration for the app server
	tlsConfig *tls.Config

	// Listener for the app server
	// This can be used for testing without having to start an actual TCP listener
	appListener net.Listener
}

// NewServerOpts contains options for the NewServer method
type NewServerOpts struct {
	AppMetrics    *metrics.AppMetrics
	TraceProvider *trace.TracerProvider

	// Jobs starts jobs and reads their status
	Jobs actors.Jobs
	// Francis rate limiter protecting browser uploads
	IngestRateLimiter RateLimiter
	// Francis rate limiter protecting the job status lookups, with a budget of its own
	StatusRateLimiter RateLimiter
	// Store where the source images and the generated variants are kept
	Store *store.Store
	// Local cache an upload writes a copy of the source into, so the job that follows doesn't download it again
	SourceCache *sourcecache.Cache
	// Measures the uploaded images
	Measurer SourceMeasurer
	// Authenticates every caller
	Authenticator auth.Authenticator
	// Decides what a browser upload may do; nil disables browser uploads
	Authorizer authorizer.Authorizer
	// Webhook endpoints notifications are resolved against; nil when none are configured
	Notifier *notify.Notifier
	// Ed25519 signing keys, published as JWKS; nil when none are configured
	SigningKeys *signing.KeySet
	// Closed once the service can run jobs, which is what /readyz reports; nil means it always can
	Ready <-chan struct{}
}

// NewServer creates a new Server object and initializes it
func NewServer(opts NewServerOpts) (*Server, error) {
	if opts.Jobs == nil {
		return nil, errors.New("option Jobs is required")
	}
	if opts.IngestRateLimiter == nil {
		return nil, errors.New("option IngestRateLimiter is required")
	}
	if opts.StatusRateLimiter == nil {
		return nil, errors.New("option StatusRateLimiter is required")
	}
	if opts.Store == nil {
		return nil, errors.New("option Store is required")
	}
	if opts.SourceCache == nil {
		return nil, errors.New("option SourceCache is required")
	}
	if opts.Measurer == nil {
		return nil, errors.New("option Measurer is required")
	}
	if opts.Authenticator == nil {
		return nil, errors.New("option Authenticator is required")
	}

	s := &Server{
		appMetrics:    opts.AppMetrics,
		traceProvider: opts.TraceProvider,
		jobs:          opts.Jobs,
		ingestLimiter: opts.IngestRateLimiter,
		statusLimiter: opts.StatusRateLimiter,
		store:         opts.Store,
		sourceCache:   opts.SourceCache,
		measurer:      opts.Measurer,
		authenticator: opts.Authenticator,
		authorizer:    opts.Authorizer,
		notifier:      opts.Notifier,
		ready:         opts.Ready,
	}

	// The configuration was validated already, so a proxy that doesn't parse here is a bug rather than an operator's mistake
	cfg := config.Get()
	s.trustedProxies = make([]netip.Prefix, 0, len(cfg.Server.TrustedProxies))
	for _, proxy := range cfg.Server.TrustedProxies {
		prefix, err := config.ParseTrustedProxy(proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy '%s': %w", proxy, err)
		}
		s.trustedProxies = append(s.trustedProxies, prefix)
	}

	// The key set doesn't change while the process runs, so it's encoded once
	if opts.SigningKeys != nil {
		var err error
		s.jwks, err = opts.SigningKeys.JWKS()
		if err != nil {
			return nil, fmt.Errorf("failed to publish the signing keys: %w", err)
		}
	}

	err := s.initAppServer()
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Server) initAppServer() (err error) {
	cfg := config.Get()

	// If "tls.path" is empty, use the folder where the config file is located
	tlsPath := cfg.Server.TLS.Path
	if tlsPath == "" {
		file := cfg.GetLoadedConfigPath()
		if file != "" {
			tlsPath = filepath.Dir(file)
		}
	}

	// Load the TLS configuration
	s.tlsConfig, s.tlsCertWatchFn, err = tlsconfig.Load(tlsPath, cfg.Server.TLS.CertPEM, cfg.Server.TLS.KeyPEM)
	if err != nil {
		return fmt.Errorf("failed to load TLS configuration: %w", err)
	}

	mux := http.NewServeMux()

	// Unauthenticated routes
	// /healthz only says the process answers, while /readyz also says it can run jobs, so a load balancer holds requests back until then
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /readyz", s.RouteReady)
	mux.HandleFunc("GET /.openapi", provinoopenapi.ServeHTTP)
	mux.HandleFunc("GET /.openapi/{document}", provinoopenapi.ServeHTTP)
	mux.HandleFunc("GET /.well-known/provino-keys.json", s.RouteJWKS)

	unauthorized := errUnauthorized.WriteResponse
	forbidden := errForbidden.WriteResponse
	requireIngest := auth.Middleware(s.authenticator, cfg.Auth.Scopes.Ingest, unauthorized, forbidden)
	requireServer := auth.Middleware(s.authenticator, cfg.Auth.Scopes.Server, unauthorized, forbidden)

	// Rate limits apply after authentication, so the budget belongs to the caller, and before any work is done: an upload's body isn't read, and a status lookup doesn't reach the actors
	// Every status lookup activates the image's actor and reads a workflow journal, so polling is held to a budget of its own, separate from the uploads'
	limitIngest := middlewareRateLimit(s.ingestLimiter)
	limitStatus := middlewareRateLimit(s.statusLimiter)

	// The public routes are the only ones browsers call, so they're the only ones that answer cross-origin requests
	public := http.NewServeMux()
	if s.authorizer != nil {
		public.Handle("POST /v1/public/ingest", requireIngest(limitIngest(http.HandlerFunc(s.RouteIngest))))
	}
	public.Handle("GET /v1/public/images/{namespace}/{imageId}/jobs/{jobId}", requireIngest(limitStatus(http.HandlerFunc(s.RoutePublicJobStatus))))
	mux.Handle("/v1/public/", middlewareCORS(cfg.Server.CORSAllowedOrigins)(public))

	// The client API is trusted with every namespace, so it's held to the server scope
	mux.Handle("POST /v1/client/upload", requireServer(http.HandlerFunc(s.RouteUpload)))
	mux.Handle("POST /v1/client/optimize", requireServer(http.HandlerFunc(s.RouteOptimize)))
	mux.Handle("GET /v1/client/images/{namespace}/{imageId}/jobs/{jobId}", requireServer(limitStatus(http.HandlerFunc(s.RouteClientJobStatus))))

	// Middlewares are listed innermost first, since each one wraps the ones before it
	middlewares := make([]httpserver.Middleware, 0, 6)
	middlewares = append(middlewares,
		// Name the request's span after the route the mux matched
		// Only the request the mux received carries the pattern, so this has to sit right around the mux
		middlewareSpanRoute,
		// Recover from panics
		sloghttp.Recovery,
		// Limit request bodies to what an upload of the maximum allowed size needs
		// This is the max upload size plus some buffer for other parts of the request
		// The upload itself is checked against the configured limit while it's being read, so this only stops a body that couldn't be valid in the first place
		httpserver.MiddlewareMaxBodySize(cfg.Images.MaxUploadSize+(1<<20)),
	)

	// A 401 is logged with its reason by the authentication middleware, so it isn't worth a line of its own
	// The root path and the favicon answer 404 to every crawler and browser that tries them, which is noise rather than a client error
	// Every other 404 is logged, since it's how a client calling the wrong path or polling an expired job shows up
	filters := []sloghttp.Filter{
		sloghttp.IgnoreStatus(http.StatusUnauthorized),
		func(w sloghttp.WrapResponseWriter, r *http.Request) bool {
			return r.URL.Path != "/" && r.URL.Path != "/favicon.ico"
		},
	}
	if cfg.Logs.OmitHealthChecks {
		filters = append(filters,
			func(w sloghttp.WrapResponseWriter, r *http.Request) bool {
				return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
			},
		)
	}

	middlewares = append(middlewares,
		// Log requests
		sloghttp.NewWithFilters(slog.Default(), filters...),
	)

	if s.traceProvider != nil {
		middlewares = append(middlewares,
			otelhttp.NewMiddleware("provino", otelhttp.WithTracerProvider(s.traceProvider)),
		)
	}

	// The client's address is resolved before anything records it, so the request log and the traces both show the client behind a trusted proxy, and never an address a client made up
	middlewares = append(middlewares,
		middlewareClientIP(s.trustedProxies),
	)

	s.handler = httpserver.Use(mux, middlewares...)

	return nil
}

// RouteReady answers 204 once the service can run jobs, which is when the actor host has registered with the actor runtime, and 503 until then
func (s *Server) RouteReady(w http.ResponseWriter, r *http.Request) {
	if s.ready != nil {
		select {
		case <-s.ready:
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// RouteJWKS serves the public keys of the Ed25519 keys this service signs with, so receivers can verify signatures and pick up rotated keys
func (s *Server) RouteJWKS(w http.ResponseWriter, r *http.Request) {
	if len(s.jwks) == 0 {
		http.NotFound(w, r)
		return
	}

	w.Header().Set(httpserver.HeaderContentType, "application/jwk-set+json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(s.jwks)
}

// Write the response for a failed request, keeping the internal details out of it unless they're part of an API error
func (s *Server) respondWithError(w http.ResponseWriter, r *http.Request, err error) {
	apiErr, ok := errors.AsType[httpserver.ApiError](err)
	if ok {
		apiErr.WriteResponse(w, r)
		return
	}

	slog.ErrorContext(r.Context(), "Failed to handle the request", slog.Any("error", err))
	errInternal.WriteResponse(w, r)
}

// Run the web server
// Note this function is blocking, and will return only when the server is shut down via context cancellation.
func (s *Server) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("server is already running")
	}
	defer s.running.Store(false)
	defer s.wg.Wait()

	// Start the app server before registering its shutdown, so a listener that couldn't be created returns its error instead of leaving the deferred wait blocked on a server that never started
	appSrvErrCh := make(chan error, 1)
	err := s.startAppServer(ctx, appSrvErrCh)
	if err != nil {
		return fmt.Errorf("failed to start app server: %w", err)
	}
	s.wg.Add(1)
	defer func() {
		// Handle graceful shutdown
		defer s.wg.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := s.appSrv.Shutdown(shutdownCtx)
		shutdownCancel()
		if err != nil {
			// Log the error only (could be context canceled)
			slog.WarnContext(shutdownCtx,
				"App server shutdown error",
				slog.Any("error", err),
			)
		}
	}()

	// If we have a tlsCertWatchFn, invoke that
	if s.tlsCertWatchFn != nil {
		err = s.tlsCertWatchFn(ctx)
		if err != nil {
			return fmt.Errorf("failed to watch for TLS certificates: %w", err)
		}
	}

	// Block until the context is canceled or the app server exits unexpectedly.
	select {
	case <-ctx.Done():
	case err = <-appSrvErrCh:
		return fmt.Errorf("app server failed: %w", err)
	}

	// Servers are stopped with deferred calls
	return nil
}

func (s *Server) startAppServer(ctx context.Context, appSrvErrCh chan<- error) error {
	cfg := config.Get()

	// Create the HTTP(S) server
	addr := net.JoinHostPort(cfg.Server.Bind, strconv.Itoa(cfg.Server.Port))
	s.appSrv = &http.Server{
		Addr:              addr,
		MaxHeaderBytes:    1 << 20, // 1MB
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       60 * time.Second,
		Handler:           s.handler,
	}

	// Create the listener if we don't have one already
	if s.appListener == nil {
		var err error
		// Configure TLS for the HTTP server (optional)
		if s.tlsConfig != nil {
			s.appSrv.TLSConfig = s.tlsConfig
		}

		s.appListener, err = net.Listen("tcp", s.appSrv.Addr) //nolint:noctx
		if err != nil {
			return fmt.Errorf("failed to create TCP listener: %w", err)
		}

		slog.InfoContext(ctx, "Starting app server",
			slog.String("bind", cfg.Server.Bind),
			slog.Int("port", cfg.Server.Port),
			slog.Bool("tls", s.tlsConfig != nil),
		)
	}

	// Start the HTTP(S) server in a background goroutine
	go func() {
		defer s.appListener.Close() //nolint:errcheck

		// Next call blocks until the server is shut down
		var srvErr error
		if s.tlsConfig != nil {
			srvErr = s.appSrv.ServeTLS(s.appListener, "", "")
		} else {
			srvErr = s.appSrv.Serve(s.appListener)
		}
		if !errors.Is(srvErr, http.ErrServerClosed) {
			select {
			case appSrvErrCh <- srvErr:
			default:
			}
		}
	}()

	return nil
}
