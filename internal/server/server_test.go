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

// fakeJobs records the jobs handlers queue and answers status lookups from a table
type fakeJobs struct {
	mu         sync.Mutex
	enqueued   []actors.JobRequest
	enqueueErr error
	statuses   map[string]fakeJobStatus
}

type fakeJobStatus struct {
	status    actors.JobStatus
	ownerHash string
}

func (f *fakeJobs) Enqueue(_ context.Context, req actors.JobRequest) (actors.EnqueueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.enqueueErr != nil {
		return actors.EnqueueResult{}, f.enqueueErr
	}
	f.enqueued = append(f.enqueued, req)
	return actors.EnqueueResult{JobID: uuid.NewV7().String(), Position: len(f.enqueued) - 1}, nil
}

func (f *fakeJobs) Status(_ context.Context, namespace string, imageID string, jobID string) (actors.JobStatus, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	s, ok := f.statuses[namespace+"/"+imageID+"/"+jobID]
	return s.status, s.ownerHash, ok, nil
}

func (f *fakeJobs) last(t *testing.T) actors.JobRequest {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.enqueued, "no job was queued")
	return f.enqueued[len(f.enqueued)-1]
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
}

type testServerOptions struct {
	// Leaves browser uploads disabled
	noAuthorizer bool
	// Ed25519 signing keys to publish
	signingKeys *signing.KeySet
	configure   func(c *config.Config)
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
		store:   imageStore,
		cache:   cache,
		limiter: &stubRateLimiter{allowed: true},
	}

	serverOpts := NewServerOpts{
		Jobs:          ts.jobs,
		RateLimiter:   ts.limiter,
		Store:         imageStore,
		SourceCache:   cache,
		Measurer:      imaging.NewEncoder(),
		Authenticator: authenticators,
		Notifier:      notifier,
		SigningKeys:   opts.signingKeys,
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
		header.Set("Content-Disposition", `form-data; name="image"; filename="photo.png"`)
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

func TestJWKSRoute(t *testing.T) {
	keys, err := signing.LoadKeySet(config.ConfigSigningEd25519{ActiveKey: "k1", Keys: []config.ConfigSigningKey{{ID: "k1", PrivateKey: "whsk_" + strings.Repeat("A", 43) + "="}}})
	require.NoError(t, err)
	ts := newTestServer(t, testServerOptions{signingKeys: keys})

	rec := ts.do(t, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/provino-keys.json", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/jwk-set+json", rec.Header().Get("Content-Type"))

	set := decodeBody[webhook.JWKS](t, rec)
	require.Len(t, set.Keys, 1)
	assert.Equal(t, "k1", set.Keys[0].Kid)
	_, err = set.Ed25519Keys()
	require.NoError(t, err)
}

func TestIngest(t *testing.T) {
	t.Run("stores the image and queues its job as the authorizer decides", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		ts.authz.decision.Notify = &webhook.Notify{Context: json.RawMessage(`{"from":"authorizer"}`)}

		body, contentType := uploadBody(t, client.IngestMetadata{Context: json.RawMessage(`{"site":"s1","profile":"hero"}`)}, pngImage(t, 400, 300))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/public/ingest", browserKey, body, contentType))
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		accepted := decodeBody[client.JobAccepted](t, rec)
		assert.Equal(t, "default", accepted.Namespace)
		assert.Equal(t, client.JobQueued, accepted.Status)
		imageID := uuid.MustParse(accepted.ImageID)
		assert.Equal(t, ts.store.OriginalKey("default", imageID, "png"), accepted.SourceKey)
		assert.Equal(t, ts.store.ManifestKey("default", imageID), accepted.ManifestKey)

		// The authorizer was asked about this caller, with the context and what the browser said about the file
		require.Len(t, ts.authz.requests, 1)
		asked := ts.authz.requests[0]
		assert.Equal(t, "browser-1", asked.Subject)
		assert.JSONEq(t, `{"site":"s1","profile":"hero"}`, string(asked.Context))
		assert.Equal(t, "photo.png", asked.FileName)
		assert.Equal(t, "image/jpeg", asked.ContentType)

		// The job carries the decision, the original's measurements, and who started it
		job := ts.jobs.last(t)
		assert.Equal(t, "default", job.Namespace)
		assert.Equal(t, accepted.ImageID, job.ImageID)
		require.NotNil(t, job.Source)
		assert.Equal(t, "png", job.Source.Format)
		assert.Equal(t, "photo.png", job.Source.Name)
		assert.Equal(t, 400, job.Source.Width)
		assert.Equal(t, 300, job.Source.Height)
		require.Len(t, job.Variants, 1)
		assert.Equal(t, "100x100.webp", job.Variants[0].Name)
		assert.Equal(t, 5, job.Variants[0].Effort)
		require.NotNil(t, job.Notify)
		assert.Equal(t, "app", job.Notify.Endpoint, "the default endpoint is resolved when the job is queued")
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
		"a job that can't be queued": {
			prepare: func(ts *testServer) { ts.jobs.enqueueErr = errors.New("actor host unavailable") },
			status:  http.StatusInternalServerError, code: "internal",
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
	t.Run("stores the image and queues its job", func(t *testing.T) {
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

	t.Run("an upload without variants is still a job", func(t *testing.T) {
		ts := newTestServer(t, testServerOptions{})
		body, contentType := uploadBody(t, client.UploadMetadata{Namespace: "tenant-1"}, pngImage(t, 10, 10))
		rec := ts.do(t, newRequest(t, http.MethodPost, "/v1/client/upload", serverKey, body, contentType))
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		assert.Empty(t, ts.jobs.last(t).Variants)
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
			assert.Empty(t, ts.jobs.enqueued)
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

	// The request is queued without touching storage, so an image that doesn't exist is the job's problem, not this request's
	t.Run("queues a job without touching storage", func(t *testing.T) {
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
		"a full queue": {
			request: client.OptimizeRequest{Namespace: "x", ImageID: uuid.NewV4().String(), Variants: []variant.Spec{{Format: "png", Size: variant.Resolution{Width: 1}}}},
			prepare: func(ts *testServer) { ts.jobs.enqueueErr = actors.ErrQueueFull },
			status:  http.StatusTooManyRequests, code: "queue_full",
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
		status:    actors.JobStatus{JobID: browserJob, Namespace: "default", ImageID: imageID, Status: actors.StatusQueued, Position: 2},
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
	assert.Equal(t, 2, decodeBody[client.JobStatus](t, rec).Position)

	// A browser sees its own job, and nothing else exists as far as it can tell
	rec = get("public", browserKey, browserJob)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, client.JobQueued, decodeBody[client.JobStatus](t, rec).Status)
	assert.Equal(t, http.StatusNotFound, get("public", browser2Key, browserJob).Code)
	assert.Equal(t, http.StatusNotFound, get("public", browserKey, backendJob).Code)

	assert.Equal(t, http.StatusNotFound, get("client", serverKey, uuid.NewV7().String()).Code)
	assert.Equal(t, http.StatusBadRequest, get("client", serverKey, uuid.NewV4().String()).Code)
	assert.Equal(t, http.StatusForbidden, get("client", browserKey, backendJob).Code)
	assert.Equal(t, http.StatusUnauthorized, get("public", "", browserJob).Code)
}
