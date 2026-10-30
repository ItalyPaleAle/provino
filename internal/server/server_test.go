//go:build unit

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/italypaleale/provino/client"
	"github.com/italypaleale/provino/internal/actors"
	"github.com/italypaleale/provino/internal/auth"
	"github.com/italypaleale/provino/internal/authorizer"
	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/internal/imaging"
	"github.com/italypaleale/provino/internal/notify"
	"github.com/italypaleale/provino/internal/signing"
	"github.com/italypaleale/provino/internal/sourcecache"
	"github.com/italypaleale/provino/internal/store"
	"github.com/italypaleale/provino/manifest"
	"github.com/italypaleale/provino/variant"
	"github.com/italypaleale/provino/webhook"
)

// API keys the tests authenticate with
const (
	serverKey   = "pvk_server0123456789abcdefghijklmnop"
	browserKey  = "pvk_browser0123456789abcdefghijklmno"
	browser2Key = "pvk_browser20123456789abcdefghijklmn"
	testOrigin  = "https://app.example"
)

// fakeJobs records the jobs handlers start and answers status lookups from a table
type fakeJobs struct {
	mu       sync.Mutex
	started  []actors.JobRequest
	startErr error
	statuses map[string]fakeJobStatus
	// Claims on image IDs by image, holding the token of the upload that holds each
	claims map[string]string
	// Every claim that was requested, granted or not
	claimed int
	// Every status lookup that reached the jobs
	statusCalls int
}

func (f *fakeJobs) ClaimUpload(_ context.Context, namespace string, imageID string, token string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.claimed++
	if ttl <= 0 {
		return false, errors.New("the claim needs a positive TTL")
	}
	if f.claims == nil {
		f.claims = map[string]string{}
	}
	key := namespace + "/" + imageID
	_, held := f.claims[key]
	if held {
		return false, nil
	}
	f.claims[key] = token
	return true, nil
}

func (f *fakeJobs) ReleaseUpload(_ context.Context, namespace string, imageID string, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := namespace + "/" + imageID
	if f.claims[key] == token {
		delete(f.claims, key)
	}
	return nil
}

// Report whether an upload holds a claim on an image, in any namespace
func (f *fakeJobs) holdsClaim(imageID uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for key := range f.claims {
		if strings.HasSuffix(key, "/"+imageID.String()) {
			return true
		}
	}
	return false
}

type fakeJobStatus struct {
	status    actors.JobStatus
	ownerHash string
}

func (f *fakeJobs) Start(_ context.Context, req actors.JobRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.startErr != nil {
		return "", f.startErr
	}
	f.started = append(f.started, req)
	return uuid.NewV7().String(), nil
}

func (f *fakeJobs) Status(_ context.Context, namespace string, imageID string, jobID string) (actors.JobStatus, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.statusCalls++
	s, ok := f.statuses[namespace+"/"+imageID+"/"+jobID]
	return s.status, s.ownerHash, ok, nil
}

func (f *fakeJobs) last(t *testing.T) actors.JobRequest {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.started, "no job was started")
	return f.started[len(f.started)-1]
}

// fakeAuthorizer answers every upload with the same decision, and records what it was asked
type fakeAuthorizer struct {
	mu       sync.Mutex
	decision authorizer.Decision
	err      error
	requests []authorizer.Request
}

func (f *fakeAuthorizer) Authorize(_ context.Context, req authorizer.Request) (authorizer.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, req)
	return f.decision, f.err
}

type testServer struct {
	*Server

	jobs    *fakeJobs
	authz   *fakeAuthorizer
	store   *store.Store
	cache   *sourcecache.Cache
	limiter *stubRateLimiter
	// Limiter of the job status lookups, which has a budget of its own
	statusLimiter *stubRateLimiter
}

type testServerOptions struct {
	// Leaves browser uploads disabled
	noAuthorizer bool
	// Ed25519 signing keys to publish
	signingKeys *signing.KeySet
	// Closed once the service can run jobs
	ready <-chan struct{}
	// Tracer provider the requests are traced with
	traceProvider *sdktrace.TracerProvider
	configure     func(c *config.Config)
}

func newTestServer(t *testing.T, opts testServerOptions) *testServer {
	t.Helper()

	dir := t.TempDir()
	t.Cleanup(config.SetTestConfig(func(c *config.Config) {
		c.Server.CORSAllowedOrigins = []string{testOrigin}
		c.Auth.APIKeys = []config.ConfigAPIKey{
			{ID: "server", Key: serverKey, Subject: "backend", Scopes: []string{"server"}},
			{ID: "browser", Key: browserKey, Subject: "browser-1", Scopes: []string{"ingest"}},
			{ID: "browser2", Key: browser2Key, Subject: "browser-2", Scopes: []string{"ingest"}},
		}
		c.Notify.Endpoints = map[string]config.ConfigNotifyEndpoint{"app": {
			URL:     "https://app.example/hooks",
			Timeout: time.Second,
			Auth:    config.ConfigOutboundAuth{Type: config.OutboundAuthHMAC, HMAC: config.ConfigOutboundHMAC{Secrets: []string{"whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"}}},
		}}
		c.Notify.DefaultEndpoint = "app"
		if opts.configure != nil {
			opts.configure(c)
		}
	}))
	cfg := config.Get()

	err := imaging.Startup(t.Context(), imaging.Options{Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)

	imageStore, err := store.New(store.NewOpts{OriginalsDSN: filepath.Join(dir, "originals"), VariantsDSN: filepath.Join(dir, "variants")})
	require.NoError(t, err)
	cache, err := sourcecache.New(sourcecache.Options{Source: imageStore, Dir: filepath.Join(dir, "sources"), MaxSize: cfg.Images.MaxUploadSize, MaxAge: time.Hour, Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	notifier, err := notify.New(cfg.Notify, nil)
	require.NoError(t, err)
	authenticators, err := auth.NewSet(t.Context(), cfg.Auth, cfg.Server.PublicURL)
	require.NoError(t, err)

	ts := &testServer{
		jobs: &fakeJobs{statuses: map[string]fakeJobStatus{}},
		authz: &fakeAuthorizer{decision: authorizer.Decision{
			Namespace:      "default",
			AllowedFormats: config.SourceFormats,
			Variants:       []variant.Spec{{Format: variant.FormatWebP, Quality: 80, Effort: new(5), Size: variant.Resolution{Width: 100, Height: 100}, Fit: variant.FitContain}},
		}},
		store:         imageStore,
		cache:         cache,
		limiter:       &stubRateLimiter{allowed: true},
		statusLimiter: &stubRateLimiter{allowed: true},
	}

	serverOpts := NewServerOpts{
		Jobs:              ts.jobs,
		IngestRateLimiter: ts.limiter,
		StatusRateLimiter: ts.statusLimiter,
		Store:             imageStore,
		SourceCache:       cache,
		Measurer:          imaging.NewEncoder(),
		Authenticator:     authenticators,
		Notifier:          notifier,
		SigningKeys:       opts.signingKeys,
		Ready:             opts.ready,
		TraceProvider:     opts.traceProvider,
	}
	if !opts.noAuthorizer {
		serverOpts.Authorizer = ts.authz
	}
	ts.Server, err = NewServer(serverOpts)
	require.NoError(t, err)

	return ts
}

// Serve one request and return the recorded response
func (ts *testServer) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	return rec
}

// Build a request with an API key
func newRequest(t *testing.T, method string, path string, key string, body io.Reader, contentType string) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), method, path, body)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

// Encode a PNG of the given size
func pngImage(t *testing.T, width int, height int) []byte {
	t.Helper()

	img, err := vips.Black(width, height)
	require.NoError(t, err)
	defer img.Close()
	buf, _, err := img.ExportPng(vips.NewPngExportParams())
	require.NoError(t, err)
	return buf
}

// Build a multipart upload, with the metadata part first unless it's nil
func uploadBody(t *testing.T, metadata any, image []byte) (*bytes.Buffer, string) {
	t.Helper()
	return uploadBodyWithDisposition(t, metadata, image, `form-data; name="image"; filename="photo.png"`)
}

// Build a multipart upload whose image part has the given Content-Disposition
func uploadBodyWithDisposition(t *testing.T, metadata any, image []byte, disposition string) (*bytes.Buffer, string) {
	t.Helper()

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if metadata != nil {
		data, err := json.Marshal(metadata)
		require.NoError(t, err)
		err = mw.WriteField("metadata", string(data))
		require.NoError(t, err)
	}
	if image != nil {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", disposition)
		header.Set("Content-Type", "image/jpeg")
		part, err := mw.CreatePart(header)
		require.NoError(t, err)
		_, err = part.Write(image)
		require.NoError(t, err)
	}
	err := mw.Close()
	require.NoError(t, err)

	return body, mw.FormDataContentType()
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	var out T
	err := json.Unmarshal(rec.Body.Bytes(), &out)
	require.NoError(t, err, rec.Body.String())
	return out
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	return decodeBody[client.Error](t, rec).Code
}

func TestUnauthenticatedRoutes(t *testing.T) {
	ts := newTestServer(t, testServerOptions{})

	rec := ts.do(t, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	for path, contentType := range map[string]string{
		"/.openapi":              "application/json",
		"/.openapi/provino.json": "application/json",
		"/.openapi/provino.yaml": "application/yaml",
	} {
		rec = ts.do(t, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, rec.Code, path)
		assert.Equal(t, contentType, rec.Header().Get("Content-Type"), path)
	}

	// Without signing keys there's nothing to publish
	rec = ts.do(t, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/provino-keys.json", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestReadyRoute(t *testing.T) {
	ready := make(chan struct{})
	ts := newTestServer(t, testServerOptions{ready: ready})
	get := func(path string) int {
		return ts.do(t, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)).Code
	}

	// Until the actor host is ready the service answers, but can't run jobs
	assert.Equal(t, http.StatusNoContent, get("/healthz"))
	assert.Equal(t, http.StatusServiceUnavailable, get("/readyz"))

	close(ready)
	assert.Equal(t, http.StatusNoContent, get("/readyz"))

	// Without a readiness signal the service is always ready
	ts = newTestServer(t, testServerOptions{})
	assert.Equal(t, http.StatusNoContent, get("/readyz"))
}

// Spans are named after the route that served them, and record the client behind a trusted proxy rather than the proxy or a made-up address
func TestRequestTracing(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ts := newTestServer(t, testServerOptions{traceProvider: provider, configure: func(c *config.Config) {
		c.Server.TrustedProxies = []string{"10.0.0.0/8"}
	}})

	// Serve one request and return the span it produced
	trace := func(t *testing.T, req *http.Request) sdktrace.ReadOnlySpan {
		t.Helper()
		before := len(recorder.Ended())
		ts.do(t, req)
		ended := recorder.Ended()
		require.Len(t, ended, before+1)
		return ended[len(ended)-1]
	}
	attribute := func(span sdktrace.ReadOnlySpan, key string) string {
		for _, attr := range span.Attributes() {
			if string(attr.Key) == key {
				return attr.Value.String()
			}
		}
		return ""
	}

	imageID := uuid.NewV4().String()
	jobID := uuid.NewV7().String()

	t.Run("a route of the top-level mux", func(t *testing.T) {
		span := trace(t, newRequest(t, http.MethodGet, "/v1/client/images/default/"+imageID+"/jobs/"+jobID, serverKey, nil, ""))
		assert.Equal(t, "GET /v1/client/images/{namespace}/{imageId}/jobs/{jobId}", span.Name())
		assert.Equal(t, "/v1/client/images/{namespace}/{imageId}/jobs/{jobId}", attribute(span, "http.route"))
	})

	t.Run("a route of the nested public mux", func(t *testing.T) {
		span := trace(t, newRequest(t, http.MethodGet, "/v1/public/images/default/"+imageID+"/jobs/"+jobID, browserKey, nil, ""))
		assert.Equal(t, "GET /v1/public/images/{namespace}/{imageId}/jobs/{jobId}", span.Name())
	})

	t.Run("a path no route matches", func(t *testing.T) {
		span := trace(t, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/nothing-here", nil))
		assert.Equal(t, "GET", span.Name())
		assert.Empty(t, attribute(span, "http.route"))
	})

	t.Run("the client behind a trusted proxy", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
		req.RemoteAddr = "10.1.2.3:5555"
		req.Header.Set("X-Forwarded-For", "192.0.2.99, 203.0.113.5")
		span := trace(t, req)
		assert.Equal(t, "203.0.113.5", attribute(span, "client.address"))
	})

	t.Run("a client claiming another address", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
		req.RemoteAddr = "198.51.100.7:1234"
		req.Header.Set("X-Forwarded-For", "203.0.113.5")
		span := trace(t, req)
		assert.Equal(t, "198.51.100.7", attribute(span, "client.address"))
	})
}

// The request log records 404s, which is how a wrong path or an expired job shows up, but not the root path every crawler tries, nor 401s, which are logged with their reason
func TestRequestLog(t *testing.T) {
	var buf safeBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})
	ts := newTestServer(t, testServerOptions{configure: func(c *config.Config) {
		c.Server.TrustedProxies = []string{"10.0.0.0/8"}
	}})

	// Serve one request and report the request log lines it produced
	logged := func(t *testing.T, req *http.Request) []map[string]any {
		t.Helper()
		buf.Reset()
		ts.do(t, req)

		var lines []map[string]any
		for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var entry map[string]any
			err := json.Unmarshal([]byte(line), &entry)
			require.NoError(t, err, line)
			_, isRequestLog := entry["request"]
			if isRequestLog {
				lines = append(lines, entry)
			}
		}
		return lines
	}

	assert.Empty(t, logged(t, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)), "the root path is not logged")
	assert.Empty(t, logged(t, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/favicon.ico", nil)), "the favicon is not logged")
	assert.Empty(t, logged(t, newRequest(t, http.MethodPost, "/v1/client/optimize", "", nil, "")), "a 401 is not logged")

	// A path no route matches is logged, with the client a trusted proxy names
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/client/nothing-here", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	lines := logged(t, req)
	require.Len(t, lines, 1)
	request, ok := lines[0]["request"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "/v1/client/nothing-here", request["path"])
	assert.Equal(t, "203.0.113.5", request["ip"])
	response, ok := lines[0]["response"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, http.StatusNotFound, response["status"])
}

// safeBuffer is a buffer the request log can write to while a test reads it
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p) //nolint:wrapcheck // This is the writer the logger calls, so its error is passed through as it is
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *safeBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func TestRunFailsWhenThePortIsTaken(t *testing.T) {
	// Hold a port so the server can't listen on it
	occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()                      //nolint:errcheck
	port := occupied.Addr().(*net.TCPAddr).Port //nolint:forcetypeassert

	ts := newTestServer(t, testServerOptions{configure: func(c *config.Config) {
		c.Server.Bind = "127.0.0.1"
		c.Server.Port = port
	}})

	// Run has to report the failure rather than block on a server that never started
	errCh := make(chan error, 1)
	go func() {
		errCh <- ts.Run(t.Context())
	}()
	select {
	case err = <-errCh:
		require.ErrorContains(t, err, "failed to start app server")
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after failing to listen")
	}
}

func TestJWKSRoute(t *testing.T) {
	keys, err := signing.LoadKeySet(config.ConfigSigningEd25519{Keys: []string{"whsk_" + strings.Repeat("A", 43) + "="}})
	require.NoError(t, err)
	ts := newTestServer(t, testServerOptions{signingKeys: keys})

	rec := ts.do(t, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/provino-keys.json", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/jwk-set+json", rec.Header().Get("Content-Type"))

	set := decodeBody[webhook.JWKS](t, rec)
	require.Len(t, set.Keys, 1)
	assert.Equal(t, keys.ActiveKeyID(), set.Keys[0].Kid)
	_, err = set.Ed25519Keys()
	require.NoError(t, err)
}

func TestIngest(t *testing.T) {
	t.Run("stores the image and starts its job as the authorizer decides", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		ts.authz.decision.Notify = &webhook.Notify{Context: json.RawMessage(`{"from":"authorizer"}`)}

		body, contentType := uploadBody(t, client.IngestMetadata{Context: json.RawMessage(`{"site":"s1","profile":"hero"}`)}, pngImage(t, 400, 300))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/public/ingest", browserKey, body, contentType))
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		accepted := decodeBody[client.JobAccepted](t, rec)
		assert.Equal(t, "default", accepted.Namespace)
		assert.Equal(t, client.JobRunning, accepted.Status)
		imageID := uuid.MustParse(accepted.ImageID)
		assert.Equal(t, ts.store.OriginalKey("default", imageID, "png"), accepted.SourceKey)
		assert.Equal(t, ts.store.ManifestKey("default", imageID), accepted.ManifestKey)

		// The authorizer was asked about this caller, with the context the browser sent
		require.Len(t, ts.authz.requests, 1)
		asked := ts.authz.requests[0]
		assert.Equal(t, "browser-1", asked.Subject)
		assert.JSONEq(t, `{"site":"s1","profile":"hero"}`, string(asked.Context))

		// The job carries the decision, the original's measurements, and who started it
		job := ts.jobs.last(t)
		assert.Equal(t, "default", job.Namespace)
		assert.Equal(t, accepted.ImageID, job.ImageID)
		require.NotNil(t, job.Source)
		assert.Equal(t, "png", job.Source.Format)
		assert.Equal(t, 400, job.Source.Width)
		assert.Equal(t, 300, job.Source.Height)
		require.Len(t, job.Variants, 1)
		assert.Equal(t, "100x100.webp", job.Variants[0].Name)
		assert.Equal(t, 5, job.Variants[0].Effort)
		require.NotNil(t, job.Notify)
		assert.Equal(t, "app", job.Notify.Endpoint, "the default endpoint is resolved when the job is requested")
		assert.Equal(t, subjectHash("browser-1"), job.SubjectHash)

		// The original and the manifest that records it are stored, and the cache holds a copy for the job
		_, err := ts.store.OpenOriginal(t.Context(), "default", imageID, "png", true)
		require.NoError(t, err)
		content, err := ts.store.ReadManifest(t.Context(), "default", imageID)
		require.NoError(t, err)
		m, err := manifest.Parse(content)
		require.NoError(t, err)
		assert.Equal(t, manifest.StatusStored, m.Status)
		assert.Equal(t, 400, m.SourceWidth)
		assert.Equal(t, accepted.SourceKey, m.SourceKey)
		_, err = os.Stat(ts.cache.Path("default", imageID))
		require.NoError(t, err)
	})

	t.Run("the metadata part is optional", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		body, contentType := uploadBody(t, nil, pngImage(t, 10, 10))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/public/ingest", browserKey, body, contentType))
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		assert.Empty(t, ts.authz.requests[0].Context)
	})

	t.Run("uses the image ID the authorizer names", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		imageID := uuid.NewV4()
		ts.authz.decision.ImageID = imageID

		body, contentType := uploadBody(t, nil, pngImage(t, 10, 10))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/public/ingest", browserKey, body, contentType))
		require.Equal(t, http.StatusAccepted, rec.Code)
		assert.Equal(t, imageID.String(), decodeBody[client.JobAccepted](t, rec).ImageID)

		// The same ID can't be spent twice
		body, contentType = uploadBody(t, nil, pngImage(t, 10, 10))
		rec = ts.do(t, newRequest(t, http.MethodPost, "/v1/public/ingest", browserKey, body, contentType))
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Equal(t, "image_exists", errorCode(t, rec))
	})

	failures := map[string]struct {
		prepare  func(ts *testServer)
		key      string
		metadata any
		image    []byte
		status   int
		code     string
	}{
		"no credentials":             {status: http.StatusUnauthorized, code: "unauthorized"},
		"a caller without the scope": {key: serverKey, status: http.StatusForbidden, code: "forbidden"},
		"an authorizer refusal": {
			prepare: func(ts *testServer) { ts.authz.err = authorizer.ErrRefused },
			status:  http.StatusForbidden, code: "upload_refused",
		},
		"an authorizer failure": {
			prepare: func(ts *testServer) { ts.authz.err = errors.New("unreachable") },
			status:  http.StatusBadGateway, code: "authorizer_failed",
		},
		"an authorizer asking for an unknown endpoint": {
			prepare: func(ts *testServer) { ts.authz.decision.Notify = &webhook.Notify{Endpoint: "missing"} },
			status:  http.StatusBadGateway, code: "authorizer_failed",
		},
		"a format the authorizer doesn't allow": {
			prepare: func(ts *testServer) { ts.authz.decision.AllowedFormats = []string{"jpg"} },
			status:  http.StatusUnsupportedMediaType, code: "unsupported_format",
		},
		"a file that isn't an image, whatever its name": {
			image:  []byte("GIF89a not what we accept"),
			status: http.StatusUnsupportedMediaType, code: "unsupported_format",
		},
		"an image with a valid signature that doesn't decode": {
			image:  append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}, []byte("garbage")...),
			status: http.StatusUnsupportedMediaType, code: "unreadable_image",
		},
		"an image larger than the authorizer allows": {
			prepare: func(ts *testServer) { ts.authz.decision.MaxBytes = 64 },
			status:  http.StatusRequestEntityTooLarge, code: "image_too_large",
		},
		"a context that isn't an object": {
			metadata: map[string]any{"context": []int{1, 2}},
			status:   http.StatusBadRequest, code: "invalid_metadata",
		},
		"no image": {
			image:  []byte{},
			status: http.StatusBadRequest, code: "missing_image",
		},
		"a job that can't be started": {
			prepare: func(ts *testServer) { ts.jobs.startErr = errors.New("actor host unavailable") },
			status:  http.StatusInternalServerError, code: "internal",
		},
		"an image with another job running": {
			prepare: func(ts *testServer) { ts.jobs.startErr = actors.ErrImageBusy },
			status:  http.StatusConflict, code: "image_busy",
		},
	}
	for name, tt := range failures {
		t.Run("rejects "+name, func(t *testing.T) {
			ts := newTestServer(t, testServerOptions{})
			if tt.prepare != nil {
				tt.prepare(ts)
			}
			imageID := uuid.NewV4()
			ts.authz.decision.ImageID = imageID
			key := browserKey
			if tt.key != "" {
				key = tt.key
			}
			if tt.status == http.StatusUnauthorized {
				key = ""
			}
			image := tt.image
			if image == nil {
				image = pngImage(t, 200, 200)
			}
			if len(image) == 0 {
				image = nil
			}

			body, contentType := uploadBody(t, tt.metadata, image)
			rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/public/ingest", key, body, contentType))
			assert.Equal(t, tt.status, rec.Code, rec.Body.String())
			assert.Equal(t, tt.code, errorCode(t, rec))

			// A rejected upload leaves nothing behind
			exists, err := ts.store.OriginalExists(t.Context(), "default", imageID, "png")
			require.NoError(t, err)
			assert.False(t, exists)
			_, err = ts.store.ReadManifest(t.Context(), "default", imageID)
			require.ErrorIs(t, err, store.ErrNotFound)
			entries, err := os.ReadDir(ts.cache.Dir())
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}

	t.Run("rejects an image with more pixels than allowed, after storing it", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{configure: func(c *config.Config) { c.Images.MaxSourcePixels = 100 * 100 }})
		imageID := uuid.NewV4()
		ts.authz.decision.ImageID = imageID

		body, contentType := uploadBody(t, nil, pngImage(t, 101, 100))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/public/ingest", browserKey, body, contentType))
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		assert.Equal(t, "image_too_many_pixels", errorCode(t, rec))

		exists, err := ts.store.OriginalExists(t.Context(), "default", imageID, "png")
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("is rate limited per caller", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		ts.limiter.allowed = false
		ts.limiter.retryAfter = time.Second

		body, contentType := uploadBody(t, nil, pngImage(t, 10, 10))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/public/ingest", browserKey, body, contentType))
		assert.Equal(t, http.StatusTooManyRequests, rec.Code)
		assert.Equal(t, "1", rec.Header().Get("Retry-After"))
		require.Len(t, ts.limiter.keys, 1)
		assert.True(t, strings.HasPrefix(ts.limiter.keys[0], "sub-"))
		assert.Empty(t, ts.authz.requests, "a rate-limited upload never reaches the authorizer")
	})

	t.Run("is disabled without an authorizer", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{noAuthorizer: true})
		body, contentType := uploadBody(t, nil, pngImage(t, 10, 10))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/public/ingest", browserKey, body, contentType))
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

// Browsers call only the public routes, so only those answer cross-origin requests
func TestCORSOnlyCoversThePublicRoutes(t *testing.T) {
	ts := newTestServer(t, testServerOptions{})

	preflight := func(path string, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, path, nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		return ts.do(t, req)
	}

	rec := preflight("/v1/public/ingest", testOrigin)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, testOrigin, rec.Header().Get("Access-Control-Allow-Origin"))

	rec = preflight("/v1/public/ingest", "https://elsewhere.example")
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = preflight("/v1/client/upload", testOrigin)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestClientUpload(t *testing.T) {
	t.Run("stores the image and starts its job", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		imageID := uuid.NewV4()
		metadata := client.UploadMetadata{
			Namespace: "tenant-1",
			ImageID:   imageID.String(),
			Variants:  []variant.Spec{{Format: "jpg", Quality: 80, Size: variant.Resolution{Width: 50}}},
			Notify:    &webhook.Notify{Endpoint: "app", Context: json.RawMessage(`{"k":"v"}`)},
		}

		body, contentType := uploadBody(t, metadata, pngImage(t, 80, 60))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/client/upload", serverKey, body, contentType))
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		accepted := decodeBody[client.JobAccepted](t, rec)
		assert.Equal(t, imageID.String(), accepted.ImageID)
		assert.Equal(t, "tenant-1", accepted.Namespace)

		job := ts.jobs.last(t)
		require.Len(t, job.Variants, 1)
		assert.Equal(t, imaging.FormatJPEG, job.Variants[0].Format)
		assert.Equal(t, "50xauto.jpg", job.Variants[0].Name)
		assert.JSONEq(t, `{"k":"v"}`, string(job.Notify.Context))
		// A backend's job carries no subject, so no browser can read it
		assert.Empty(t, job.SubjectHash)
	})

	// New images get a v4 ID, but an image that already has a v7 one keeps it
	t.Run("keeps a v7 image ID", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		imageID := uuid.NewV7()
		body, contentType := uploadBody(t, client.UploadMetadata{Namespace: "tenant-1", ImageID: imageID.String()}, pngImage(t, 10, 10))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/client/upload", serverKey, body, contentType))
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		assert.Equal(t, imageID.String(), decodeBody[client.JobAccepted](t, rec).ImageID)
	})

	// The original is stored under the image's ID, so the part's file name is never read: it can be missing, or longer than a job's input can hold
	t.Run("ignores the image's file name", func(t *testing.T) {
		dispositions := map[string]string{
			"no file name":   `form-data; name="image"`,
			"long file name": `form-data; name="image"; filename="` + strings.Repeat("a", 100<<10) + `.png"`,
		}
		for name, disposition := range dispositions {
			t.Run(name, func(t *testing.T) {
				ts := newTestServer(t, testServerOptions{})
				body, contentType := uploadBodyWithDisposition(t, client.UploadMetadata{Namespace: "tenant-1"}, pngImage(t, 10, 10), disposition)
				rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/client/upload", serverKey, body, contentType))
				require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			})
		}
	})

	t.Run("an upload without variants is still a job", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		body, contentType := uploadBody(t, client.UploadMetadata{Namespace: "tenant-1"}, pngImage(t, 10, 10))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/client/upload", serverKey, body, contentType))
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		assert.Empty(t, ts.jobs.last(t).Variants)

		// A minted ID is new, so it isn't claimed
		assert.Zero(t, ts.jobs.claimed)
	})

	t.Run("an image ID is spent by its first upload, whatever the format", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		imageID := uuid.NewV4()
		metadata := client.UploadMetadata{Namespace: "tenant-1", ImageID: imageID.String()}

		body, contentType := uploadBody(t, metadata, pngImage(t, 10, 10))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/client/upload", serverKey, body, contentType))
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		// The claim is released once the job has started
		assert.False(t, ts.jobs.holdsClaim(imageID))

		// The second upload is a JPEG, whose original would be stored under another key
		img, err := vips.Black(10, 10)
		require.NoError(t, err)
		defer img.Close()
		jpeg, _, err := img.ExportJpeg(vips.NewJpegExportParams())
		require.NoError(t, err)
		body, contentType = uploadBody(t, metadata, jpeg)
		rec = ts.do(t, newRequest(t, http.MethodPost, "/v1/client/upload", serverKey, body, contentType))
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Equal(t, "image_exists", errorCode(t, rec))
		assert.Len(t, ts.jobs.started, 1)
		assert.False(t, ts.jobs.holdsClaim(imageID))

		// Nothing of the second upload was stored
		exists, err := ts.store.OriginalExists(t.Context(), "tenant-1", imageID, "jpg")
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("an image ID another upload is storing is refused", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		imageID := uuid.NewV4()
		ts.jobs.claims = map[string]string{"tenant-1/" + imageID.String(): "another-upload"}

		body, contentType := uploadBody(t, client.UploadMetadata{Namespace: "tenant-1", ImageID: imageID.String()}, pngImage(t, 10, 10))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/client/upload", serverKey, body, contentType))
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Equal(t, "image_exists", errorCode(t, rec))
		assert.Empty(t, ts.jobs.started)

		// The other upload's claim is left alone, and nothing was stored
		assert.True(t, ts.jobs.holdsClaim(imageID))
		exists, err := ts.store.ImageExists(t.Context(), "tenant-1", imageID)
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("a failed upload releases its claim", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		ts.jobs.startErr = errors.New("actor host unavailable")
		imageID := uuid.NewV4()

		body, contentType := uploadBody(t, client.UploadMetadata{Namespace: "tenant-1", ImageID: imageID.String()}, pngImage(t, 10, 10))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/client/upload", serverKey, body, contentType))
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		assert.Equal(t, 1, ts.jobs.claimed)
		assert.False(t, ts.jobs.holdsClaim(imageID))

		// The ID is usable again, since the failed upload removed what it wrote
		exists, err := ts.store.ImageExists(t.Context(), "tenant-1", imageID)
		require.NoError(t, err)
		assert.False(t, exists)
	})

	failures := map[string]struct {
		key      string
		metadata any
		status   int
		code     string
	}{
		"a browser key":           {key: browserKey, metadata: client.UploadMetadata{Namespace: "x"}, status: http.StatusForbidden, code: "forbidden"},
		"no metadata":             {status: http.StatusBadRequest, code: "invalid_metadata"},
		"an invalid namespace":    {metadata: client.UploadMetadata{Namespace: "Not/Valid"}, status: http.StatusBadRequest, code: "invalid_namespace"},
		"a v1 image ID":           {metadata: client.UploadMetadata{Namespace: "x", ImageID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"}, status: http.StatusBadRequest, code: "invalid_image_id"},
		"an invalid variant":      {metadata: client.UploadMetadata{Namespace: "x", Variants: []variant.Spec{{Format: "webp"}}}, status: http.StatusBadRequest, code: "invalid_variants"},
		"an unknown endpoint":     {metadata: client.UploadMetadata{Namespace: "x", Notify: &webhook.Notify{Endpoint: "missing"}}, status: http.StatusBadRequest, code: "invalid_notify"},
		"a context not an object": {metadata: client.UploadMetadata{Namespace: "x", Notify: &webhook.Notify{Context: json.RawMessage(`"text"`)}}, status: http.StatusBadRequest, code: "invalid_notify"},
	}
	for name, tt := range failures {
		t.Run("rejects "+name, func(t *testing.T) {
			ts := newTestServer(t, testServerOptions{})
			key := serverKey
			if tt.key != "" {
				key = tt.key
			}
			body, contentType := uploadBody(t, tt.metadata, pngImage(t, 10, 10))
			rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/client/upload", key, body, contentType))
			assert.Equal(t, tt.status, rec.Code, rec.Body.String())
			assert.Equal(t, tt.code, errorCode(t, rec))
			assert.Empty(t, ts.jobs.started)
		})
	}
}

func TestOptimize(t *testing.T) {
	optimize := func(t *testing.T, ts *testServer, req any) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(req)
		require.NoError(t, err)
		return ts.do(t, newRequest(t, http.MethodPost, "/v1/client/optimize", serverKey, bytes.NewReader(data), "application/json"))
	}

	// The job is started without touching storage, so an image that doesn't exist is the job's problem, not this request's
	t.Run("starts a job without touching storage", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		imageID := uuid.NewV7()
		rec := optimize(t, ts, client.OptimizeRequest{
			Namespace:    "tenant-1",
			ImageID:      imageID.String(),
			Variants:     []variant.Spec{{Format: "webp", Quality: 80, Size: variant.Resolution{Width: 100, Height: 100}}},
			SourceFormat: ".JPEG",
		})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		accepted := decodeBody[client.JobAccepted](t, rec)
		assert.Equal(t, imageID.String(), accepted.ImageID)
		assert.Empty(t, accepted.SourceKey)

		job := ts.jobs.last(t)
		assert.Equal(t, "jpg", job.SourceFormat)
		assert.Nil(t, job.Source)
		require.Len(t, job.Variants, 1)
	})

	failures := map[string]struct {
		request any
		prepare func(ts *testServer)
		status  int
		code    string
	}{
		"no variants":            {request: client.OptimizeRequest{Namespace: "x", ImageID: uuid.NewV4().String()}, status: http.StatusBadRequest, code: "invalid_variants"},
		"an invalid image ID":    {request: client.OptimizeRequest{Namespace: "x", ImageID: "nope"}, status: http.StatusBadRequest, code: "invalid_image_id"},
		"an unknown format":      {request: client.OptimizeRequest{Namespace: "x", ImageID: uuid.NewV4().String(), SourceFormat: "gif", Variants: []variant.Spec{{Format: "png", Size: variant.Resolution{Width: 1}}}}, status: http.StatusBadRequest, code: "invalid_source_format"},
		"conflicting variants":   {request: client.OptimizeRequest{Namespace: "x", ImageID: uuid.NewV4().String(), Variants: []variant.Spec{{Format: "jpg", Quality: 80, Size: variant.Resolution{Width: 1}}, {Format: "jpg", Quality: 70, Size: variant.Resolution{Width: 1}}}}, status: http.StatusBadRequest, code: "invalid_variants"},
		"a body that isn't JSON": {request: "not an object", status: http.StatusBadRequest, code: "invalid_body"},
		"an image with another job running": {
			request: client.OptimizeRequest{Namespace: "x", ImageID: uuid.NewV4().String(), Variants: []variant.Spec{{Format: "png", Size: variant.Resolution{Width: 1}}}},
			prepare: func(ts *testServer) { ts.jobs.startErr = actors.ErrImageBusy },
			status:  http.StatusConflict, code: "image_busy",
		},
	}
	for name, tt := range failures {
		t.Run("rejects "+name, func(t *testing.T) {
			ts := newTestServer(t, testServerOptions{})
			if tt.prepare != nil {
				tt.prepare(ts)
			}
			rec := optimize(t, ts, tt.request)
			assert.Equal(t, tt.status, rec.Code, rec.Body.String())
			assert.Equal(t, tt.code, errorCode(t, rec))
		})
	}
}

func TestJobStatusRoutes(t *testing.T) {
	ts := newTestServer(t, testServerOptions{})
	imageID := uuid.NewV4().String()
	browserJob := uuid.NewV7().String()
	backendJob := uuid.NewV7().String()
	ts.jobs.statuses["default/"+imageID+"/"+browserJob] = fakeJobStatus{
		status:    actors.JobStatus{JobID: browserJob, Namespace: "default", ImageID: imageID, Status: actors.StatusRunning, Total: 3, Completed: 1},
		ownerHash: subjectHash("browser-1"),
	}
	ts.jobs.statuses["default/"+imageID+"/"+backendJob] = fakeJobStatus{
		status: actors.JobStatus{JobID: backendJob, Namespace: "default", ImageID: imageID, Status: actors.StatusFailed, Error: "image_not_found: gone"},
	}
	get := func(scope string, key string, job string) *httptest.ResponseRecorder {
		return ts.do(t, newRequest(t, http.MethodGet, "/v1/"+scope+"/images/default/"+imageID+"/jobs/"+job, key, nil, ""))
	}

	rec := get("client", serverKey, backendJob)
	require.Equal(t, http.StatusOK, rec.Code)
	status := decodeBody[client.JobStatus](t, rec)
	assert.Equal(t, client.JobFailed, status.Status)
	assert.Equal(t, "image_not_found: gone", status.Error)

	// The client API sees every job of an image, including those browsers started
	rec = get("client", serverKey, browserJob)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, decodeBody[client.JobStatus](t, rec).Completed)

	// A browser sees its own job, and nothing else exists as far as it can tell
	rec = get("public", browserKey, browserJob)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, client.JobRunning, decodeBody[client.JobStatus](t, rec).Status)
	assert.Equal(t, http.StatusNotFound, get("public", browser2Key, browserJob).Code)
	assert.Equal(t, http.StatusNotFound, get("public", browserKey, backendJob).Code)

	assert.Equal(t, http.StatusNotFound, get("client", serverKey, uuid.NewV7().String()).Code)
	assert.Equal(t, http.StatusBadRequest, get("client", serverKey, uuid.NewV4().String()).Code)
	assert.Equal(t, http.StatusForbidden, get("client", browserKey, backendJob).Code)
	assert.Equal(t, http.StatusUnauthorized, get("public", "", browserJob).Code)

	// Every lookup reaches the actors, so both routes are held to the status budget, keyed on the caller, and a throttled lookup never reaches them
	t.Run("are rate limited per caller", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		ts.statusLimiter.allowed = false
		ts.statusLimiter.retryAfter = time.Second

		for _, route := range []struct{ scope, key string }{{"public", browserKey}, {"client", serverKey}} {
			rec := ts.do(t, newRequest(t, http.MethodGet, "/v1/"+route.scope+"/images/default/"+imageID+"/jobs/"+browserJob, route.key, nil, ""))
			assert.Equal(t, http.StatusTooManyRequests, rec.Code, route.scope)
			assert.Equal(t, "1", rec.Header().Get("Retry-After"), route.scope)
		}
		require.Len(t, ts.statusLimiter.keys, 2)
		assert.NotEqual(t, ts.statusLimiter.keys[0], ts.statusLimiter.keys[1])
		assert.Zero(t, ts.jobs.statusCalls, "a throttled lookup never reaches the actors")

		// Uploads have a budget of their own, which polling doesn't spend
		assert.Empty(t, ts.limiter.keys)
	})
}
