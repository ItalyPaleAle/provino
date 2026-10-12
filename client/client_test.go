package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/italypaleale/provino/variant"
)

const testKey = "pvk_0123456789abcdefghijklmnopqrstuv" //nolint:gosec // A test fixture, not a credential

func newTestClient(t *testing.T, handler http.HandlerFunc, opts ...Option) *Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	if len(opts) == 0 {
		opts = []Option{WithAPIKey(testKey)}
	}
	c, err := New(server.URL+"/", opts...)
	require.NoError(t, err)
	return c
}

func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) //nolint:errchkjson // The test values always encode
}

func TestUploadStreamsTheMetadataFirst(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/client/upload", r.URL.Path)
		assert.Equal(t, "Bearer "+testKey, r.Header.Get("Authorization"))

		mr, err := r.MultipartReader()
		if !assert.NoError(t, err) {
			return
		}

		// The metadata comes first, because provino needs it before it reads the image
		part, err := mr.NextPart()
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "metadata", part.FormName())
		var metadata UploadMetadata
		err = json.NewDecoder(part).Decode(&metadata)
		assert.NoError(t, err)
		assert.Equal(t, "tenant-1", metadata.Namespace)
		assert.Len(t, metadata.Variants, 1)

		part, err = mr.NextPart()
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "image", part.FormName())
		assert.Equal(t, `my "photo".jpg`, part.FileName())
		image, err := io.ReadAll(part)
		assert.NoError(t, err)
		assert.Equal(t, "image bytes", string(image))

		respond(w, http.StatusAccepted, JobAccepted{Namespace: "tenant-1", ImageID: "img", JobID: "job", Status: JobQueued})
	})

	accepted, err := c.Upload(t.Context(), UploadMetadata{
		Namespace: "tenant-1",
		Variants:  []variant.Spec{{Format: variant.FormatWebP, Quality: 80, Size: variant.Resolution{Width: 100}}},
	}, `my "photo".jpg`, strings.NewReader("image bytes"))
	require.NoError(t, err)
	assert.Equal(t, "job", accepted.JobID)
}

func TestOptimizeAndErrors(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		var req OptimizeRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		assert.NoError(t, err)

		if req.ImageID == "full" {
			respond(w, http.StatusTooManyRequests, Error{Code: "queue_full", Message: "The image already has the maximum number of jobs waiting"})
			return
		}
		respond(w, http.StatusAccepted, JobAccepted{ImageID: req.ImageID, JobID: "job", Status: JobQueued, Position: 1})
	})

	accepted, err := c.Optimize(t.Context(), OptimizeRequest{Namespace: "n", ImageID: "img"})
	require.NoError(t, err)
	assert.Equal(t, 1, accepted.Position)

	_, err = c.Optimize(t.Context(), OptimizeRequest{Namespace: "n", ImageID: "full"})
	require.Error(t, err)
	assert.True(t, IsCode(err, "queue_full"))
	apiErr, ok := err.(*APIError) //nolint:errorlint // The client returns the error itself, unwrapped
	require.True(t, ok)
	assert.Equal(t, http.StatusTooManyRequests, apiErr.StatusCode)
}

func TestUnexpectedResponsesAreStillAPIErrors(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream down")
	})

	_, err := c.Job(t.Context(), "n", "img", "job")
	require.Error(t, err)
	assert.True(t, IsCode(err, "unexpected_response"))
	assert.Contains(t, err.Error(), "upstream down")
}

func TestWaitForJobPollsUntilTheJobEnds(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/client/images/tenant-1/img/jobs/job", r.URL.Path)
		state := JobRunning
		if calls.Add(1) >= 3 {
			state = JobCompleted
		}
		respond(w, http.StatusOK, JobStatus{JobID: "job", Status: state})
	})

	status, err := c.WaitForJob(t.Context(), "tenant-1", "img", "job", 10*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, JobCompleted, status.Status)
	assert.EqualValues(t, 3, calls.Load())
}

func TestTokenSourceCredentials(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token-1", r.Header.Get("Authorization"))
		respond(w, http.StatusOK, JobStatus{Status: JobCompleted})
	}, WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "token-1", TokenType: "Bearer"})))

	_, err := c.Job(t.Context(), "n", "img", "job")
	require.NoError(t, err)
}

func TestNewValidates(t *testing.T) {
	_, err := New("not a url", WithAPIKey(testKey))
	require.Error(t, err)

	_, err = New("https://images.example.com")
	require.ErrorContains(t, err, "credentials")
}
