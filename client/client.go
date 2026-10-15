// Package client calls provino's client API, which trusted backends use to upload images, request variants for existing ones, and follow the jobs that do the work
//
// It also defines the request and response types of the HTTP API, which the server itself uses, so the two never disagree about the wire format
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Maximum size of a response body the client reads, which bounds what a misbehaving server can make it allocate
const maxResponseSize = 1 << 20 // 1 MB

// Client calls provino's client API
type Client struct {
	baseURL   string
	http      *http.Client
	authorize func(req *http.Request) error
}

// Option configures a Client
type Option func(*Client)

// WithHTTPClient sets the HTTP client requests are sent with
// Defaults to one with a 5 minute timeout so a large upload has time to finish
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) {
		cl.http = c
	}
}

// WithAPIKey authenticates every request with an API key
func WithAPIKey(key string) Option {
	return func(cl *Client) {
		cl.authorize = func(req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+key)
			return nil
		}
	}
}

// WithTokenSource authenticates every request with a token from an OAuth2 token source, such as a client-credentials grant
// Wrap the source in oauth2.ReuseTokenSource so a token is reused until it expires
func WithTokenSource(source oauth2.TokenSource) Option {
	return func(cl *Client) {
		cl.authorize = func(req *http.Request) error {
			token, err := source.Token()
			if err != nil {
				return fmt.Errorf("failed to obtain an access token: %w", err)
			}
			token.SetAuthHeader(req)
			return nil
		}
	}
}

// New creates a client for the provino instance at baseURL, for example `https://images.example.com`
func New(baseURL string, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || !u.IsAbs() || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("the base URL '%s' is not an absolute HTTP URL", baseURL)
	}

	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Minute},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.authorize == nil {
		return nil, errors.New("the client needs credentials: use WithAPIKey or WithTokenSource")
	}

	return c, nil
}

// APIError is returned when provino answers with an error status
type APIError struct {
	// HTTP status of the response
	StatusCode int
	// Body of the response
	Body Error
}

// Error implements the error interface
func (e *APIError) Error() string {
	msg := fmt.Sprintf("provino returned %d %s: %s", e.StatusCode, e.Body.Code, e.Body.Message)
	if e.Body.InnerError != "" {
		msg += " (" + e.Body.InnerError + ")"
	}
	return msg
}

// IsCode reports whether err is an APIError with the given code, for example `queue_full`
func IsCode(err error, code string) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	return ok && apiErr.Body.Code == code
}

// Upload stores an image and queues the job that generates its variants
// The image is streamed and its format is detected by provino from its content
func (c *Client) Upload(ctx context.Context, metadata UploadMetadata, filename string, image io.Reader) (JobAccepted, error) {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return JobAccepted{}, fmt.Errorf("failed to encode the metadata: %w", err)
	}

	// The body is written as it's sent, with the metadata first because provino reads the parts in order
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := writeUpload(mw, encoded, filename, image)
		_ = pw.CloseWithError(err)
	}()

	var accepted JobAccepted
	err = c.do(ctx, http.MethodPost, "/v1/client/upload", mw.FormDataContentType(), pr, http.StatusAccepted, &accepted)
	_ = pr.Close()
	if err != nil {
		return JobAccepted{}, err
	}

	return accepted, nil
}

// Write the multipart body of an upload
func writeUpload(mw *multipart.Writer, metadata []byte, filename string, image io.Reader) error {
	err := mw.WriteField("metadata", string(metadata))
	if err != nil {
		return fmt.Errorf("failed to write the metadata part: %w", err)
	}

	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="image"; filename="`+escapeQuotes(filename)+`"`)
	header.Set("Content-Type", "application/octet-stream")

	part, err := mw.CreatePart(header)
	if err != nil {
		return fmt.Errorf("failed to write the image part: %w", err)
	}

	_, err = io.Copy(part, image)
	if err != nil {
		return fmt.Errorf("failed to copy the image: %w", err)
	}

	err = mw.Close()
	if err != nil {
		return fmt.Errorf("failed to finish the upload body: %w", err)
	}

	return nil
}

// Optimize queues a job that generates variants for an image that's already stored
func (c *Client) Optimize(ctx context.Context, req OptimizeRequest) (JobAccepted, error) {
	encoded, err := json.Marshal(req)
	if err != nil {
		return JobAccepted{}, fmt.Errorf("failed to encode the request: %w", err)
	}

	var accepted JobAccepted
	err = c.do(ctx, http.MethodPost, "/v1/client/optimize", "application/json", bytes.NewReader(encoded), http.StatusAccepted, &accepted)
	if err != nil {
		return JobAccepted{}, err
	}

	return accepted, nil
}

// Job returns the status of a job
func (c *Client) Job(ctx context.Context, namespace string, imageID string, jobID string) (JobStatus, error) {
	path := "/v1/client/images/" + url.PathEscape(namespace) + "/" + url.PathEscape(imageID) + "/jobs/" + url.PathEscape(jobID)

	var status JobStatus
	err := c.do(ctx, http.MethodGet, path, "", nil, http.StatusOK, &status)

	if err != nil {
		return JobStatus{}, err
	}

	return status, nil
}

// DefaultPollInterval is how often WaitForJob polls when it's given no interval
const DefaultPollInterval = time.Second

// WaitForJob polls a job until it completes or fails, returning its final status
// An interval of zero or less polls every DefaultPollInterval
// Prefer a webhook when one is configured: it reports the outcome without a request every interval
func (c *Client) WaitForJob(ctx context.Context, namespace string, imageID string, jobID string, interval time.Duration) (JobStatus, error) {
	// A ticker panics on an interval that isn't positive, which a caller that leaves it unset shouldn't hit
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		status, err := c.Job(ctx, namespace, imageID, jobID)
		if err != nil {
			return JobStatus{}, err
		}
		if status.Status == JobCompleted || status.Status == JobFailed {
			return status, nil
		}

		select {
		case <-ctx.Done():
			return status, fmt.Errorf("stopped waiting for the job: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Send a request and decode a successful answer into out
func (c *Client) do(ctx context.Context, method string, path string, contentType string, body io.Reader, wantStatus int, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("failed to create the request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	err = c.authorize(req)
	if err != nil {
		return err
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send the request: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck

	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("failed to read the response: %w", err)
	}

	if res.StatusCode != wantStatus {
		apiErr := &APIError{
			StatusCode: res.StatusCode,
		}
		decodeErr := json.Unmarshal(data, &apiErr.Body)
		if decodeErr != nil || apiErr.Body.Code == "" {
			apiErr.Body = Error{
				Code:    "unexpected_response",
				Message: strings.TrimSpace(string(data)),
			}
		}
		return apiErr
	}

	err = json.Unmarshal(data, out)
	if err != nil {
		return fmt.Errorf("failed to decode the response: %w", err)
	}

	return nil
}

func escapeQuotes(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}
