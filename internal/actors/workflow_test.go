package actors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/italypaleale/francis/actor"
	francisworkflow "github.com/italypaleale/francis/builtin/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/internal/config"
	"github.com/italypaleale/provino/internal/imaging"
	"github.com/italypaleale/provino/internal/notify"
	"github.com/italypaleale/provino/internal/store"
	"github.com/italypaleale/provino/manifest"
	"github.com/italypaleale/provino/webhook"
)

// How long the tests wait for a job to terminate
// The deadline remains an alarm, so host startup and polling still need some headroom
const workflowWaitTimeout = 30 * time.Second

// The namespace every job in these tests belongs to, which is the first segment of every key they write
const testNamespace = "6ba7b810-9dad-41d1-80b4-00c04fd430c8"

// The secret test webhook endpoints sign with
const testWebhookSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw" //nolint:gosec // A test fixture, not a credential

// testCompensation supplies the durable values a compensation handler is allowed to read
type testCompensation struct {
	instanceID string
	step       string
	input      any
	item       any
	result     any
	outputs    map[string]any
}

func (c testCompensation) InstanceID() string { return c.instanceID }
func (c testCompensation) Workflow() string   { return jobWorkflowName }
func (c testCompensation) Step() string       { return c.step }
func (c testCompensation) Index() int         { return -1 }
func (c testCompensation) Attempt() int       { return 1 }
func (c testCompensation) Cause() string      { return "test failure" }

func (c testCompensation) DecodeInput(into any) error {
	return decodeTestValue(c.input, into)
}

func (c testCompensation) DecodeItem(into any) error {
	return decodeTestValue(c.item, into)
}

func (c testCompensation) DecodeOutput(step string, into any) error {
	value, ok := c.outputs[step]
	if !ok {
		return francisworkflow.ErrStepNotFound
	}
	return decodeTestValue(value, into)
}

func (c testCompensation) DecodeResult(into any) error {
	return decodeTestValue(c.result, into)
}

func decodeTestValue(value any, into any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to encode the test value: %w", err)
	}
	err = json.Unmarshal(data, into)
	if err != nil {
		return fmt.Errorf("failed to decode the test value: %w", err)
	}
	return nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()

	dir := t.TempDir()
	imageStore, err := store.New(store.NewOpts{
		OriginalsDSN: filepath.Join(dir, "originals"),
		VariantsDSN:  filepath.Join(dir, "variants"),
	})
	require.NoError(t, err)
	return imageStore
}

func webpSpec(name string, width int, height int) VariantSpec {
	return VariantSpec{Name: name, Format: imaging.FormatWebP, Quality: 80, Effort: 5, Resolution: imaging.Resolution{Width: width, Height: height}, Fit: imaging.FitContain}
}

// A provino/v1 manifest describing one stored 100x75 WebP file
func previousManifest(imageID uuid.UUID) manifest.Manifest {
	return manifest.Manifest{
		Version:      manifest.Version,
		Namespace:    testNamespace,
		ImageID:      imageID.String(),
		Status:       manifest.StatusCompleted,
		SourceFormat: "png",
		Variants: []manifest.Variant{{
			Name: "100x100.webp", Format: imaging.FormatWebP, Quality: 80, Resolution: imaging.Resolution{Width: 100, Height: 100},
			Fit: imaging.FitContain, Width: 100, Height: 75, Size: 9,
		}},
	}
}

func TestCompensations(t *testing.T) {
	imageStore := newTestStore(t)
	handlers := &jobWorkflowHandlers{runtime: &Runtime{Store: imageStore, Log: slog.New(slog.DiscardHandler)}}
	imageID := uuid.NewV4()
	task := func(spec VariantSpec) variantTask {
		return variantTask{Namespace: testNamespace, ImageID: imageID.String(), Spec: spec}
	}

	t.Run("a generated variant is removed", func(t *testing.T) {
		err := imageStore.SaveVariant(t.Context(), testNamespace, imageID, 200, 150, "webp", []byte("new"), "image/webp")
		require.NoError(t, err)

		compensation := testCompensation{
			step:    stepGenerate,
			item:    task(webpSpec("200x200.webp", 200, 200)),
			result:  VariantResult{Width: 200, Height: 150},
			outputs: map[string]any{stepSnapshot: jobSnapshot{}},
		}
		err = handlers.deleteVariant(t.Context(), compensation)
		require.NoError(t, err)
		err = handlers.deleteVariant(t.Context(), compensation)
		require.NoError(t, err)

		_, err = imageStore.ReadVariant(t.Context(), testNamespace, imageID, 200, 150, "webp")
		require.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("a reused variant is kept", func(t *testing.T) {
		entry := manifest.Variant{Format: imaging.FormatWebP, Width: 100, Height: 75, Size: 9}
		err := imageStore.SaveVariant(t.Context(), testNamespace, imageID, 100, 75, "webp", []byte("old"), "image/webp")
		require.NoError(t, err)

		err = handlers.deleteVariant(t.Context(), testCompensation{
			step:    stepGenerate,
			item:    task(webpSpec("100x100.webp", 100, 100)),
			result:  VariantResult{Width: 100, Height: 75, Reused: &entry},
			outputs: map[string]any{stepSnapshot: jobSnapshot{}},
		})
		require.NoError(t, err)

		content, err := imageStore.ReadVariant(t.Context(), testNamespace, imageID, 100, 75, "webp")
		require.NoError(t, err)
		assert.Equal(t, "old", string(content))
	})

	// A re-encode writes the same key the previous manifest lists, and that manifest is what the image is left with, so the file stays
	t.Run("a variant the previous manifest references is kept", func(t *testing.T) {
		err := imageStore.SaveVariant(t.Context(), testNamespace, imageID, 100, 75, "webp", []byte("re-encoded"), "image/webp")
		require.NoError(t, err)
		previous, err := json.Marshal(previousManifest(imageID))
		require.NoError(t, err)

		err = handlers.deleteVariant(t.Context(), testCompensation{
			step:    stepGenerate,
			item:    task(webpSpec("100x100.webp", 100, 100)),
			result:  VariantResult{Width: 100, Height: 75},
			outputs: map[string]any{stepSnapshot: jobSnapshot{Previous: previous}},
		})
		require.NoError(t, err)

		_, err = imageStore.ReadVariant(t.Context(), testNamespace, imageID, 100, 75, "webp")
		require.NoError(t, err)
	})

	t.Run("the manifest the job found is put back", func(t *testing.T) {
		previous, err := json.Marshal(previousManifest(imageID))
		require.NoError(t, err)
		err = imageStore.WriteManifest(t.Context(), testNamespace, imageID, map[string]string{"version": manifest.Version, "written": "by the failing job"})
		require.NoError(t, err)

		compensation := testCompensation{
			step:    stepManifest,
			input:   JobRequest{Namespace: testNamespace, ImageID: imageID.String(), Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}},
			outputs: map[string]any{stepSnapshot: jobSnapshot{Previous: previous}},
		}
		err = handlers.restoreManifest(t.Context(), compensation)
		require.NoError(t, err)

		content, err := imageStore.ReadManifest(t.Context(), testNamespace, imageID)
		require.NoError(t, err)
		assert.JSONEq(t, string(previous), string(content))
	})

	t.Run("an image that had no manifest is left without one", func(t *testing.T) {
		err := imageStore.WriteManifest(t.Context(), testNamespace, imageID, map[string]string{"written": "by the failing job"})
		require.NoError(t, err)

		err = handlers.restoreManifest(t.Context(), testCompensation{
			step:    stepManifest,
			input:   JobRequest{Namespace: testNamespace, ImageID: imageID.String(), Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}},
			outputs: map[string]any{stepSnapshot: jobSnapshot{}},
		})
		require.NoError(t, err)

		_, err = imageStore.ReadManifest(t.Context(), testNamespace, imageID)
		require.ErrorIs(t, err, store.ErrNotFound)
	})
}

func TestReusableVariantsRequiresTheManifestEntryAndStoredObject(t *testing.T) {
	imageStore := newTestStore(t)
	imageID := uuid.NewV4()
	existing := manifest.Variant{Name: "100x100.webp", Format: imaging.FormatWebP, Quality: 80, Resolution: imaging.Resolution{Width: 100, Height: 100}, Fit: imaging.FitContain, Width: 100, Height: 75, Size: 9}
	missing := manifest.Variant{Name: "200x200.webp", Format: imaging.FormatWebP, Quality: 80, Resolution: imaging.Resolution{Width: 200, Height: 200}, Fit: imaging.FitContain, Width: 200, Height: 150, Size: 7}
	// A skipped entry repeats another entry's file, so it never stands for a file of its own
	skipped := manifest.Variant{Name: "300x300.webp", Format: imaging.FormatWebP, Quality: 80, Resolution: imaging.Resolution{Width: 300, Height: 300}, Fit: imaging.FitContain, Width: 100, Height: 75, Size: 9, Skipped: true, SkippedFor: &imaging.Resolution{Width: 100, Height: 100}}
	for _, entry := range []manifest.Variant{existing} {
		err := imageStore.SaveVariant(t.Context(), testNamespace, imageID, entry.Width, entry.Height, entry.Format.Extension(), []byte("variant"), entry.Format.ContentType())
		require.NoError(t, err)
	}
	previous := manifest.Manifest{Version: manifest.Version, Namespace: testNamespace, ImageID: imageID.String(), Status: manifest.StatusCompleted, SourceFormat: "png", Variants: []manifest.Variant{existing, missing, skipped}}

	source := imaging.Resolution{Width: 800, Height: 600}
	handlers := &jobWorkflowHandlers{runtime: &Runtime{Store: imageStore, Log: slog.New(slog.DiscardHandler)}}
	stored, err := handlers.reusableVariants(t.Context(), testNamespace, imageID, &previous)
	require.NoError(t, err)
	assert.Equal(t, map[variantIdentity]manifest.Variant{
		{Format: existing.Format, Fit: imaging.FitContain, Width: existing.Resolution.Width, Height: existing.Resolution.Height}: existing,
	}, stored.byBox)

	// The same box with another fit is another picture, so it doesn't reuse the file
	_, found := stored.lookup(source, VariantSpec{Format: imaging.FormatWebP, Resolution: existing.Resolution, Fit: imaging.FitCover})
	assert.False(t, found)

	// A request that leaves the fit out means contain
	entry, found := stored.lookup(source, VariantSpec{Format: imaging.FormatWebP, Resolution: existing.Resolution})
	assert.True(t, found)
	assert.Equal(t, existing, entry)

	result, err := handlers.generate(t.Context(), variantTask{
		Namespace: testNamespace,
		ImageID:   imageID.String(),
		Spec:      VariantSpec{Name: existing.Name, Format: existing.Format, Resolution: existing.Resolution},
		Existing:  &existing,
	})
	require.NoError(t, err)
	assert.Equal(t, &existing, result.Reused)
	assert.Equal(t, existing.Size, result.Size)
}

// A box the source can't fill finds the full-resolution file another box stored, because that file holds the same pixels
func TestStoredVariantsLookupFindsTheFullResolutionFileOfAnotherBox(t *testing.T) {
	source := imaging.Resolution{Width: 1200, Height: 800}
	full := manifest.Variant{Format: imaging.FormatWebP, Quality: 80, Resolution: imaging.Resolution{Width: 1400, Height: 1400}, Fit: imaging.FitContain, Width: 1200, Height: 800, Size: 42}
	stored := storedVariants{
		byBox:    map[variantIdentity]manifest.Variant{{Format: imaging.FormatWebP, Fit: imaging.FitContain, Width: 1400, Height: 1400}: full},
		byOutput: map[variantIdentity]manifest.Variant{{Format: imaging.FormatWebP, Fit: imaging.FitContain, Width: 1200, Height: 800}: full},
	}

	entry, found := stored.lookup(source, VariantSpec{Format: imaging.FormatWebP, Resolution: imaging.Resolution{Width: 1300, Height: 1300}, Fit: imaging.FitContain})
	assert.True(t, found)
	assert.Equal(t, full, entry)

	// A box the source fills needs a scaled file, which no other box's file is
	_, found = stored.lookup(source, VariantSpec{Format: imaging.FormatWebP, Resolution: imaging.Resolution{Width: 1000, Height: 1000}, Fit: imaging.FitContain})
	assert.False(t, found)

	// Another format is another file
	_, found = stored.lookup(source, VariantSpec{Format: imaging.FormatAVIF, Resolution: imaging.Resolution{Width: 1300, Height: 1300}, Fit: imaging.FitContain})
	assert.False(t, found)
}

func TestImageActorNextCheck(t *testing.T) {
	a := &imageActor{runtime: &Runtime{WorkflowTimeout: 10 * time.Minute, CompletedWorkflowRetention: 24 * time.Hour}}
	now := time.Now()

	// A running job is looked at every minute rather than only after its hour-long deadline, so a lost release doesn't hold the queue that long
	due := a.nextCheck(&imageState{Active: &activeJob{StartedAt: now}}, now)
	assert.Equal(t, now.Add(activePollInterval), due)

	// Close to its deadline, the deadline comes first
	startedAt := now.Add(-jobDeadline(a.runtime.WorkflowTimeout))
	due = a.nextCheck(&imageState{Active: &activeJob{StartedAt: startedAt}}, now)
	assert.Equal(t, startedAt.Add(jobDeadline(a.runtime.WorkflowTimeout)+activeCheckMargin), due)

	// A claim is cleared once it expires
	expiresAt := now.Add(10 * time.Second)
	due = a.nextCheck(&imageState{Upload: &uploadClaim{Token: "t", ExpiresAt: expiresAt}}, now)
	assert.Equal(t, expiresAt, due)
}

func TestFailureMessage(t *testing.T) {
	assert.Equal(t, "image_not_found: there is no original", failureMessage("step snapshot failed: image_not_found: there is no original"))
	assert.Equal(t, "something else", failureMessage("something else"))
}

func TestValidateJobID(t *testing.T) {
	err := ValidateJobID(uuid.NewV7().String())
	require.NoError(t, err)

	for _, invalid := range []string{"", "not-a-uuid", uuid.NewV4().String(), strings.ToUpper(uuid.NewV7().String())} {
		err := ValidateJobID(invalid)
		require.Error(t, err, invalid)
	}
}

// Pick a port that is free right now, since the actor host requires an explicit port to advertise to its peers
func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx
	require.NoError(t, err)
	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok)
	err = l.Close()
	require.NoError(t, err)

	return addr.Port
}

// testHost is an actor host running against a SQLite database and local stores in a temporary folder
type testHost struct {
	*Host

	store *store.Store
}

type testHostOptions struct {
	// URL of the webhook endpoint named "test", if the test needs one
	webhookURL string
	// Changes to the configuration
	configure func(c *config.Config)
}

// Start an actor host backed by a SQLite database in a temporary folder
func startTestHost(t *testing.T, opts testHostOptions) *testHost {
	t.Helper()

	// The workflow peer-server lifecycle gets a separate non-race run so the repository race suite can cover the rest of this package independently
	// These tests still run in "make test-workflows"
	if raceDetectorEnabled {
		t.Skip("skipped under the race detector: the francis peer server races with itself while shutting down")
	}

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	t.Cleanup(config.SetTestConfig(func(c *config.Config) {
		c.Actors.Address = net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))
		c.Actors.Runtime = config.ActorRuntimeEmbedded
		c.Actors.Database = "sqlite://" + filepath.Join(dir, "actors.db")
		c.Actors.WorkflowTimeout = workflowWaitTimeout
		c.Storage.Originals = filepath.Join(dir, "originals")
		c.Storage.Variants = filepath.Join(dir, "variants")
		// The cache is local to a host, so each test gets its own folder rather than the shared one in the system temporary directory
		c.Storage.SourceCache = filepath.Join(dir, "sources")
		c.Auth.APIKeys = []config.ConfigAPIKey{{ID: "test", Key: "pvk_0123456789abcdefghijklmnopqrstuv"}}
		if opts.webhookURL != "" {
			c.Notify.Endpoints = map[string]config.ConfigNotifyEndpoint{"test": {
				URL:     opts.webhookURL,
				Timeout: 5 * time.Second,
				Auth:    config.ConfigOutboundAuth{Type: config.OutboundAuthHMAC, HMAC: config.ConfigOutboundHMAC{Secrets: []string{testWebhookSecret}}},
			}}
		}
		if opts.configure != nil {
			opts.configure(c)
		}
	}))

	cfg := config.Get()
	err := cfg.Validate(logger)
	require.NoError(t, err)

	imageStore, err := store.New(store.NewOpts{OriginalsDSN: cfg.Storage.Originals, VariantsDSN: cfg.Storage.Variants})
	require.NoError(t, err)

	notifier, err := notify.New(cfg.Notify, nil)
	require.NoError(t, err)

	err = imaging.Startup(t.Context(), imaging.Options{Logger: logger})
	require.NoError(t, err)

	host, err := NewHost(NewHostOpts{
		Store:    imageStore,
		Encoder:  imaging.NewEncoder(),
		Logger:   logger,
		Notifier: notifier,
		// The deadline is an alarm, so tests poll for due alarms far more often than production
		alarmsPollInterval: 200 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- host.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErrCh:
			assert.NoError(t, err)
		case <-time.After(15 * time.Second):
			t.Error("timed out waiting for the actor host to stop")
		}
	})

	select {
	case <-host.Ready():
	case err := <-runErrCh:
		require.NoError(t, err, "the actor host stopped before it was ready")
		t.Fatal("the actor host stopped before it was ready")
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the actor host to be ready")
	}

	return &testHost{Host: host, store: imageStore}
}

// Store a PNG original the way an upload does, along with the stored manifest the upload writes
func (h *testHost) upload(t *testing.T, imageID uuid.UUID, width int, height int) JobSource {
	t.Helper()

	img, err := vips.Black(width, height)
	require.NoError(t, err)
	defer img.Close()
	buf, _, err := img.ExportPng(vips.NewPngExportParams())
	require.NoError(t, err)

	_, size, err := h.store.SaveOriginal(t.Context(), testNamespace, imageID, "png", bytes.NewReader(buf), int64(len(buf)), "image/png")
	require.NoError(t, err)

	source := JobSource{Format: "png", Name: "source.png", Size: size, Width: width, Height: height, StoredAt: time.Now().UTC().Truncate(time.Second)}
	err = h.store.WriteManifest(t.Context(), testNamespace, imageID, manifest.Manifest{
		Version: manifest.Version, Namespace: testNamespace, ImageID: imageID.String(), Status: manifest.StatusStored,
		SourceKey: h.store.OriginalKey(testNamespace, imageID, "png"), SourceName: source.Name, SourceFormat: "png", SourceSize: size,
		SourceWidth: width, SourceHeight: height, StoredAt: source.StoredAt, Variants: []manifest.Variant{},
	})
	require.NoError(t, err)

	return source
}

// Queue a job and return its ID
func (h *testHost) enqueue(t *testing.T, req JobRequest) string {
	t.Helper()

	if req.Namespace == "" {
		req.Namespace = testNamespace
	}
	if req.RequestedAt.IsZero() {
		req.RequestedAt = time.Now()
	}
	res, err := h.Jobs().Enqueue(t.Context(), req)
	require.NoError(t, err)
	return res.JobID
}

// Wait for a job to reach a status, and return it
func (h *testHost) waitForJob(t *testing.T, imageID uuid.UUID, jobID string, want Status) JobStatus {
	t.Helper()

	var status JobStatus
	require.Eventually(t, func() bool {
		var found bool
		var err error
		status, _, found, err = h.Jobs().Status(t.Context(), testNamespace, imageID.String(), jobID)
		return err == nil && found && status.Status == want
	}, workflowWaitTimeout, 100*time.Millisecond, "the job did not reach status %s (last seen: %+v)", want, status)

	return status
}

// Read the image's manifest, bypassing any cache the store reads through
func (h *testHost) manifest(t *testing.T, imageID uuid.UUID) manifest.Manifest {
	t.Helper()

	content, err := h.store.ReadManifestConsistent(t.Context(), testNamespace, imageID)
	require.NoError(t, err)
	m, err := manifest.Parse(content)
	require.NoError(t, err)
	return m
}

// A webhook receiver that verifies every event and hands it to the test
type testReceiver struct {
	*httptest.Server

	events chan webhook.Event
	// Every delivery ID received, in order
	ids chan string
}

func newTestReceiver(t *testing.T, respond func(w http.ResponseWriter, event webhook.Event)) *testReceiver {
	t.Helper()

	verifier, err := webhook.NewVerifier(t.Context(), webhook.WithHMACSecrets(testWebhookSecret))
	require.NoError(t, err)

	r := &testReceiver{events: make(chan webhook.Event, 16), ids: make(chan string, 64)}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := verifier.VerifyRequest(req, 1<<20) // 1MB
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var event webhook.Event
		err = json.Unmarshal(body, &event)
		assert.NoError(t, err)
		r.ids <- req.Header.Get(webhook.HeaderID)

		if respond != nil {
			respond(w, event)
			return
		}
		r.events <- event
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *testReceiver) next(t *testing.T) webhook.Event {
	t.Helper()

	select {
	case event := <-r.events:
		return event
	case <-time.After(workflowWaitTimeout):
		t.Fatal("no event was delivered")
		return webhook.Event{}
	}
}

func TestJobGeneratesEveryVariant(t *testing.T) {
	receiver := newTestReceiver(t, nil)
	host := startTestHost(t, testHostOptions{webhookURL: receiver.URL})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 1600, 1200)
	variants := []VariantSpec{
		webpSpec("400x400.webp", 400, 400),
		{Name: "200x200-cover.avif", Format: imaging.FormatAVIF, Quality: 40, Effort: 4, Resolution: imaging.Resolution{Width: 200, Height: 200}, Fit: imaging.FitCover},
		{Name: "800xauto.jpg", Format: imaging.FormatJPEG, Quality: 85, Resolution: imaging.Resolution{Width: 800}, Fit: imaging.FitContain},
	}
	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: variants, Notify: &webhook.Notify{Endpoint: "test", Context: json.RawMessage(`{"site":"s1"}`)}})

	event := receiver.next(t)
	assert.Equal(t, webhook.EventImageOptimized, event.Type)
	assert.Equal(t, jobID, event.Data.JobID)
	assert.JSONEq(t, `{"site":"s1"}`, string(event.Data.Context))
	assert.Equal(t, host.store.ManifestKey(testNamespace, imageID), event.Data.ManifestKey)
	assert.Equal(t, jobID+".image.optimized", <-receiver.ids)

	status := host.waitForJob(t, imageID, jobID, StatusCompleted)
	assert.Equal(t, 3, status.Total)
	assert.Equal(t, 3, status.Completed)

	m := host.manifest(t, imageID)
	assert.Equal(t, manifest.StatusCompleted, m.Status)
	assert.Equal(t, jobID, m.JobID)
	assert.Equal(t, "source.png", m.SourceName)
	assert.Equal(t, source.StoredAt, m.StoredAt.UTC())
	require.Len(t, m.Variants, len(variants))
	require.NotNil(t, event.Data.Manifest)
	assert.Equal(t, m.Variants, event.Data.Manifest.Variants)

	for i, entry := range m.Variants {
		assert.Equal(t, variants[i].Name, entry.Name)
		assert.Equal(t, host.store.AssetKey(testNamespace, imageID, entry.Width, entry.Height, entry.Format.Extension()), entry.Key)

		// Every key in the manifest resolves to a stored file of the recorded size
		content, err := host.store.ReadVariant(t.Context(), testNamespace, imageID, entry.Width, entry.Height, entry.Format.Extension())
		require.NoError(t, err)
		assert.Equal(t, entry.Size, int64(len(content)))
	}

	// The source is 4:3, so fitting it in a square box makes the height the binding side
	assert.Equal(t, 400, m.Variants[0].Width)
	assert.Equal(t, 300, m.Variants[0].Height)
	// A cover variant crops the source to the exact requested box
	assert.Equal(t, 200, m.Variants[1].Width)
	assert.Equal(t, 200, m.Variants[1].Height)
	// A resolution with only a width keeps the aspect ratio of the source
	assert.Equal(t, 800, m.Variants[2].Width)
	assert.Equal(t, 600, m.Variants[2].Height)
}

// An upload without variants is still a job, which records the image and notifies
func TestJobWithoutVariantsReportsTheStoredImage(t *testing.T) {
	receiver := newTestReceiver(t, nil)
	host := startTestHost(t, testHostOptions{webhookURL: receiver.URL})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	before, err := host.store.ReadManifestConsistent(t.Context(), testNamespace, imageID)
	require.NoError(t, err)

	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Notify: &webhook.Notify{Endpoint: "test"}})

	event := receiver.next(t)
	assert.Equal(t, webhook.EventImageStored, event.Type)
	require.NotNil(t, event.Data.Manifest)
	assert.Equal(t, manifest.StatusStored, event.Data.Manifest.Status)
	assert.Equal(t, 800, event.Data.Manifest.SourceWidth)

	status := host.waitForJob(t, imageID, jobID, StatusCompleted)
	assert.Zero(t, status.Total)

	// The job wrote nothing: the manifest is the one the upload stored
	after, err := host.store.ReadManifestConsistent(t.Context(), testNamespace, imageID)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// The source is downloaded once, before any variant is scheduled, and the local copy is removed once the job terminates
func TestJobCachesTheSourceImageLocally(t *testing.T) {
	host := startTestHost(t, testHostOptions{})
	sourceCacheDir := config.Get().Storage.SourceCache

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	// Without a recorded size the job reads the header, which is what puts the source in the cache before the fan-out
	source.Width, source.Height = 0, 0
	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{
		webpSpec("100x100.webp", 100, 100),
		webpSpec("200x200.webp", 200, 200),
		webpSpec("300x300.webp", 300, 300),
	}})
	host.waitForJob(t, imageID, jobID, StatusCompleted)

	assert.Eventually(t, func() bool {
		entries, err := os.ReadDir(sourceCacheDir)
		return err == nil && len(entries) == 0
	}, workflowWaitTimeout, 100*time.Millisecond, "the job did not remove the source image it cached locally")
}

// A failed job undoes what it did, puts back the manifest it found, and reports the failure
func TestJobFailureRestoresTheImage(t *testing.T) {
	receiver := newTestReceiver(t, nil)
	host := startTestHost(t, testHostOptions{webhookURL: receiver.URL})

	// The original is not an image, which the encoder refuses permanently, so the fan-out fails without spending its retry budget
	imageID := uuid.NewV4()
	_, _, err := host.store.SaveOriginal(t.Context(), testNamespace, imageID, "png", strings.NewReader("not an image"), 1024, "image/png")
	require.NoError(t, err)
	stored := manifest.Manifest{
		Version: manifest.Version, Namespace: testNamespace, ImageID: imageID.String(), Status: manifest.StatusStored,
		SourceFormat: "png", SourceWidth: 800, SourceHeight: 600, Variants: []manifest.Variant{},
	}
	err = host.store.WriteManifest(t.Context(), testNamespace, imageID, stored)
	require.NoError(t, err)
	before, err := host.store.ReadManifestConsistent(t.Context(), testNamespace, imageID)
	require.NoError(t, err)

	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}, Notify: &webhook.Notify{Endpoint: "test"}})

	event := receiver.next(t)
	assert.Equal(t, webhook.EventImageFailed, event.Type)
	assert.NotEmpty(t, event.Data.Error)
	require.NotNil(t, event.Data.Manifest)
	assert.Equal(t, manifest.StatusStored, event.Data.Manifest.Status)

	status := host.waitForJob(t, imageID, jobID, StatusFailed)
	assert.NotEmpty(t, status.Error)

	// Nothing the job stored is left, and the manifest is exactly what it was
	_, err = host.store.ReadVariant(t.Context(), testNamespace, imageID, 100, 75, "webp")
	require.ErrorIs(t, err, store.ErrNotFound)
	after, err := host.store.ReadManifestConsistent(t.Context(), testNamespace, imageID)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after))
}

// The failure event is dated when the job failed rather than when it was requested, and keeps that date when its delivery is retried
func TestJobFailureEventIsDatedWhenTheJobFailed(t *testing.T) {
	var attempts atomic.Int32
	timestamps := make(chan time.Time, 4)
	receiver := newTestReceiver(t, func(w http.ResponseWriter, event webhook.Event) {
		timestamps <- event.Timestamp
		// The first attempt is refused, so the event is delivered again
		if attempts.Add(1) == 1 {
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	host := startTestHost(t, testHostOptions{webhookURL: receiver.URL})

	imageID := uuid.NewV4()
	requestedAt := time.Now().Add(-time.Hour)
	start := time.Now()
	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), SourceFormat: "jpg", RequestedAt: requestedAt, Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}, Notify: &webhook.Notify{Endpoint: "test"}})
	host.waitForJob(t, imageID, jobID, StatusFailed)

	var received []time.Time
	for range 2 {
		select {
		case ts := <-timestamps:
			received = append(received, ts)
		case <-time.After(workflowWaitTimeout):
			t.Fatal("the failure event was not delivered twice")
		}
	}
	assert.True(t, received[0].Equal(received[1]), "the timestamp changed across attempts: %s, %s", received[0], received[1])
	assert.False(t, received[0].Before(start.Truncate(time.Second)), "the event is dated %s, before the job ran", received[0])
}

// A job for an image that doesn't exist fails before it writes anything, and says why
func TestJobForAMissingOriginalFails(t *testing.T) {
	receiver := newTestReceiver(t, nil)
	host := startTestHost(t, testHostOptions{webhookURL: receiver.URL})

	imageID := uuid.NewV4()
	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), SourceFormat: "jpg", Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}, Notify: &webhook.Notify{Endpoint: "test"}})

	event := receiver.next(t)
	assert.Equal(t, webhook.EventImageFailed, event.Type)
	assert.True(t, strings.HasPrefix(event.Data.Error, ErrorImageNotFound), event.Data.Error)
	assert.Nil(t, event.Data.Manifest)

	status := host.waitForJob(t, imageID, jobID, StatusFailed)
	assert.True(t, strings.HasPrefix(status.Error, ErrorImageNotFound), status.Error)

	_, err := host.store.ReadManifest(t.Context(), testNamespace, imageID)
	require.ErrorIs(t, err, store.ErrNotFound)

	// An image without a manifest and a request without a format is the same: nothing to find the original by
	otherID := uuid.NewV4()
	otherJob := host.enqueue(t, JobRequest{ImageID: otherID.String(), Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}})
	status = host.waitForJob(t, otherID, otherJob, StatusFailed)
	assert.True(t, strings.HasPrefix(status.Error, ErrorImageNotFound), status.Error)
}

func TestJobCompletesWhenTheWebhookIsNeverAcknowledged(t *testing.T) {
	var attempts atomic.Int32
	receiver := newTestReceiver(t, func(w http.ResponseWriter, _ webhook.Event) {
		attempts.Add(1)
		http.Error(w, "no", http.StatusInternalServerError)
	})
	host := startTestHost(t, testHostOptions{webhookURL: receiver.URL})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}, Notify: &webhook.Notify{Endpoint: "test"}})

	// The variants and the manifest are there for the application to find, so the job completes and the undelivered event is only recorded
	host.waitForJob(t, imageID, jobID, StatusCompleted)
	assert.Equal(t, manifest.StatusCompleted, host.manifest(t, imageID).Status)

	// Delivery is retried before the optional step is given up on, and every attempt carries the same delivery ID
	assert.Greater(t, attempts.Load(), int32(1), "the event must be retried before the job gives up on it")
	first := <-receiver.ids
	second := <-receiver.ids
	assert.Equal(t, first, second)
}

// The jobs of one image run one at a time, in the order they were queued, so the last one's manifest is the one left behind
func TestJobsOfOneImageRunInOrder(t *testing.T) {
	release := make(chan struct{})
	var held atomic.Bool
	receiver := newTestReceiver(t, func(w http.ResponseWriter, event webhook.Event) {
		// The first job's delivery is held, which holds the job, so the ones after it have to wait in the queue
		if held.CompareAndSwap(false, true) {
			<-release
		}
		w.WriteHeader(http.StatusNoContent)
	})
	host := startTestHost(t, testHostOptions{webhookURL: receiver.URL, configure: func(c *config.Config) {
		c.Images.MaxQueuedJobs = 2
	}})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	notifyTest := &webhook.Notify{Endpoint: "test"}
	first := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}, Notify: notifyTest})
	require.Eventually(t, held.Load, workflowWaitTimeout, 50*time.Millisecond, "the first job did not reach its delivery")

	second, err := host.Jobs().Enqueue(t.Context(), JobRequest{Namespace: testNamespace, ImageID: imageID.String(), Variants: []VariantSpec{webpSpec("200x200.webp", 200, 200)}})
	require.NoError(t, err)
	assert.Equal(t, 1, second.Position)
	third, err := host.Jobs().Enqueue(t.Context(), JobRequest{Namespace: testNamespace, ImageID: imageID.String(), Variants: []VariantSpec{webpSpec("300x300.webp", 300, 300)}})
	require.NoError(t, err)
	assert.Equal(t, 2, third.Position)

	// The queue holds two, so a third waiting job is refused
	_, err = host.Jobs().Enqueue(t.Context(), JobRequest{Namespace: testNamespace, ImageID: imageID.String(), Variants: []VariantSpec{webpSpec("400x400.webp", 400, 400)}})
	require.ErrorIs(t, err, ErrQueueFull)

	// A waiting job reports its place in the queue, and an image that isn't this one doesn't know the job at all
	status, _, found, err := host.Jobs().Status(t.Context(), testNamespace, imageID.String(), third.JobID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, StatusQueued, status.Status)
	assert.Equal(t, 2, status.Position)
	_, _, found, err = host.Jobs().Status(t.Context(), testNamespace, uuid.NewV4().String(), third.JobID)
	require.NoError(t, err)
	assert.False(t, found)

	close(release)
	host.waitForJob(t, imageID, first, StatusCompleted)
	host.waitForJob(t, imageID, second.JobID, StatusCompleted)
	host.waitForJob(t, imageID, third.JobID, StatusCompleted)

	m := host.manifest(t, imageID)
	assert.Equal(t, third.JobID, m.JobID)
	require.Len(t, m.Variants, 1)
	assert.Equal(t, "300x300.webp", m.Variants[0].Name)
}

// A job's status says who started it, which is what lets a browser read its own jobs and nobody else's
func TestJobStatusCarriesTheSubject(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, SubjectHash: "hash-of-the-subject"})
	host.waitForJob(t, imageID, jobID, StatusCompleted)

	_, subjectHash, found, err := host.Jobs().Status(t.Context(), testNamespace, imageID.String(), jobID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "hash-of-the-subject", subjectHash)

	_, _, found, err = host.Jobs().Status(t.Context(), testNamespace, imageID.String(), uuid.NewV7().String())
	require.NoError(t, err)
	assert.False(t, found)
}

// An active job whose workflow doesn't exist, which a crash between recording and starting it leaves behind, doesn't hold the queue
func TestImageActorFreesAStaleActiveJob(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	err := host.host.SetState(t.Context(), imageActorType, imageActorID(testNamespace, imageID.String()), imageState{
		Active: &activeJob{JobID: uuid.NewV7().String(), StartedAt: time.Now()},
	}, nil)
	require.NoError(t, err)

	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}})
	host.waitForJob(t, imageID, jobID, StatusCompleted)
}

// A process that stops after recording a job as active and before starting its workflow leaves the alarm scheduled first, which starts the job rather than dropping it
func TestImageActorStartsAnActiveJobThatNeverStarted(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	jobID := uuid.NewV7().String()
	request := JobRequest{JobID: jobID, Namespace: testNamespace, ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}, RequestedAt: time.Now()}

	// This is what the actor writes before it starts a workflow: the alarm first, then the active job with its request
	actorID := imageActorID(testNamespace, imageID.String())
	err := host.host.SetAlarm(t.Context(), imageActorType, actorID, alarmCheck, actor.AlarmProperties{DueTime: time.Now()})
	require.NoError(t, err)
	err = host.host.SetState(t.Context(), imageActorType, actorID, imageState{
		Active: &activeJob{JobID: jobID, StartedAt: time.Now(), Request: &request},
	}, nil)
	require.NoError(t, err)

	// Until the alarm fires, the job reads as running, and then it runs for real
	host.waitForJob(t, imageID, jobID, StatusCompleted)
	m := host.manifest(t, imageID)
	assert.Equal(t, jobID, m.JobID)
}

// A job whose journal the retention in effect when it became active could have purged is freed rather than run again, even after the retention was raised
func TestImageActorFreesAJobWhoseJournalCouldBePurged(t *testing.T) {
	host := startTestHost(t, testHostOptions{configure: func(c *config.Config) {
		c.Actors.CompletedWorkflowRetention = 24 * time.Hour
	}})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	staleID := uuid.NewV7().String()
	request := JobRequest{JobID: staleID, Namespace: testNamespace, ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}, RequestedAt: time.Now()}

	// The job became active two hours ago, when the retention was an hour, and its start was never confirmed
	err := host.host.SetState(t.Context(), imageActorType, imageActorID(testNamespace, imageID.String()), imageState{
		Active: &activeJob{JobID: staleID, StartedAt: time.Now().Add(-2 * time.Hour), Request: &request, Retention: time.Hour},
	}, nil)
	require.NoError(t, err)

	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), Variants: []VariantSpec{webpSpec("200x200.webp", 200, 200)}})
	host.waitForJob(t, imageID, jobID, StatusCompleted)

	// The stale job never ran, so it has no status and the manifest is the new job's
	_, _, found, err := host.Jobs().Status(t.Context(), testNamespace, imageID.String(), staleID)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, jobID, host.manifest(t, imageID).JobID)
}

func TestImageActorCouldBePurged(t *testing.T) {
	now := time.Now()
	startedAt := now.Add(-2 * time.Hour)
	tests := map[string]struct {
		current  time.Duration
		recorded time.Duration
		want     bool
	}{
		"within the retention":                          {current: 24 * time.Hour, recorded: 24 * time.Hour, want: false},
		"past the retention":                            {current: time.Hour, recorded: time.Hour, want: true},
		"raised after the job became active":            {current: 24 * time.Hour, recorded: time.Hour, want: true},
		"lowered after the job became active":           {current: time.Hour, recorded: 24 * time.Hour, want: true},
		"indefinite now, but not when it became active": {current: 0, recorded: time.Hour, want: true},
		"indefinite when it became active":              {current: time.Hour, recorded: 0, want: true},
		"indefinite throughout":                         {current: 0, recorded: 0, want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := &imageActor{runtime: &Runtime{CompletedWorkflowRetention: tt.current}}
			assert.Equal(t, tt.want, a.couldBePurged(&activeJob{StartedAt: startedAt, Retention: tt.recorded}, now))
		})
	}
}

func TestRegenerationReusesExistingResolutionsAndGeneratesOnlyMissingOnes(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	first := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}})
	host.waitForJob(t, imageID, first, StatusCompleted)
	initial := host.manifest(t, imageID)
	require.Len(t, initial.Variants, 1)

	// An invalid quality proves the existing pair is never passed to the encoder while the new pair still needs the source
	invalid := webpSpec("100x100.webp", 100, 100)
	invalid.Quality = 101
	second := host.enqueue(t, JobRequest{ImageID: imageID.String(), Variants: []VariantSpec{invalid, webpSpec("200x200.webp", 200, 200)}})
	host.waitForJob(t, imageID, second, StatusCompleted)

	regenerated := host.manifest(t, imageID)
	require.Len(t, regenerated.Variants, 2)
	assert.Equal(t, 80, regenerated.Variants[0].Quality, "reused metadata must describe the stored encoding")
	assert.Equal(t, initial.Variants[0].Size, regenerated.Variants[0].Size)
	assert.Equal(t, 200, regenerated.Variants[1].Width)
	assert.Equal(t, 150, regenerated.Variants[1].Height)
	// The original's details came from the manifest the upload wrote
	assert.Equal(t, "source.png", regenerated.SourceName)
}

// No variant is larger than its source, and boxes that would only repeat the source's pixels store nothing
func TestJobNeverEnlargesSmallOriginals(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 800)
	variants := []VariantSpec{
		webpSpec("500x500.webp", 500, 500),
		webpSpec("1000x1000.webp", 1000, 1000),
		webpSpec("2000x2000.webp", 2000, 2000),
		{Name: "1200x500-cover.webp", Format: imaging.FormatWebP, Quality: 80, Effort: 5, Resolution: imaging.Resolution{Width: 1200, Height: 500}, Fit: imaging.FitCover},
		{Name: "2400x1000-cover.webp", Format: imaging.FormatWebP, Quality: 80, Effort: 5, Resolution: imaging.Resolution{Width: 2400, Height: 1000}, Fit: imaging.FitCover},
	}
	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: variants})
	host.waitForJob(t, imageID, jobID, StatusCompleted)

	m := host.manifest(t, imageID)
	require.Len(t, m.Variants, len(variants))

	// A box the source fills is scaled down as before
	assert.Equal(t, 500, m.Variants[0].Width)
	assert.False(t, m.Variants[0].Skipped)

	// The tightest box the source can't fill holds the source at its own size, and the looser box repeats its file
	holder := m.Variants[1]
	assert.Equal(t, 800, holder.Width)
	assert.Equal(t, 800, holder.Height)
	skipped := m.Variants[2]
	assert.True(t, skipped.Skipped)
	assert.Equal(t, &imaging.Resolution{Width: 1000, Height: 1000}, skipped.SkippedFor)
	assert.Equal(t, holder.Key, skipped.Key)

	// A cover box keeps its shape at the source's full width, and its larger twin is skipped
	assert.Equal(t, 800, m.Variants[3].Width)
	assert.Equal(t, 333, m.Variants[3].Height)
	assert.True(t, m.Variants[4].Skipped)

	// Nothing was stored at an enlarged size
	for _, size := range []imaging.Resolution{{Width: 1000, Height: 1000}, {Width: 2000, Height: 2000}, {Width: 1200, Height: 500}, {Width: 2400, Height: 1000}} {
		_, err := host.store.ReadVariant(t.Context(), testNamespace, imageID, size.Width, size.Height, "webp")
		require.ErrorIs(t, err, store.ErrNotFound, "found a variant enlarged to %s", size)
	}
}

// A file enlarged before provino stopped doing that is replaced by one at the source's own size
func TestRegenerationReplacesEnlargedFiles(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 800)

	// A manifest listing a 1000x1000 file enlarged from the 800x800 source
	err := host.store.SaveVariant(t.Context(), testNamespace, imageID, 1000, 1000, "webp", []byte("enlarged"), "image/webp")
	require.NoError(t, err)
	err = host.store.WriteManifest(t.Context(), testNamespace, imageID, manifest.Manifest{
		Version: manifest.Version, Namespace: testNamespace, ImageID: imageID.String(), Status: manifest.StatusCompleted,
		SourceFormat: "png", SourceWidth: 800, SourceHeight: 800,
		Variants: []manifest.Variant{{Name: "1000x1000.webp", Format: imaging.FormatWebP, Quality: 80, Effort: 5, Resolution: imaging.Resolution{Width: 1000, Height: 1000}, Fit: imaging.FitContain, Width: 1000, Height: 1000, Size: int64(len("enlarged"))}},
	})
	require.NoError(t, err)

	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec("1000x1000.webp", 1000, 1000), webpSpec("2000x2000.webp", 2000, 2000)}})
	host.waitForJob(t, imageID, jobID, StatusCompleted)

	m := host.manifest(t, imageID)
	require.Len(t, m.Variants, 2)
	assert.Equal(t, 800, m.Variants[0].Width)
	assert.NotEqual(t, int64(len("enlarged")), m.Variants[0].Size, "the enlarged file must not be reused")
	assert.True(t, m.Variants[1].Skipped)
}

// Change the quality of a variant
func withQuality(spec VariantSpec, quality int) VariantSpec {
	spec.Quality = quality
	return spec
}

// Boxes that differ can come out at the same size for one source, and then only one encoding of that file can be stored
func TestVariantsThatComeOutAsOneFile(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	t.Run("different settings fail the job and leave nothing behind", func(t *testing.T) {
		imageID := uuid.NewV4()
		source := host.upload(t, imageID, 1500, 1000)

		// For a 3:2 source, a 600x600 box and a box 600 pixels wide both come out at 600x400
		jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{
			withQuality(webpSpec("600x600.webp", 600, 600), 90),
			withQuality(webpSpec("600xauto.webp", 600, 0), 20),
		}})
		status := host.waitForJob(t, imageID, jobID, StatusFailed)
		assert.Contains(t, status.Error, "give them the same quality and effort")

		// The manifest the upload wrote is still there, and the file the job stored was removed
		m := host.manifest(t, imageID)
		assert.Equal(t, manifest.StatusStored, m.Status)
		_, err := host.store.ReadVariant(t.Context(), testNamespace, imageID, 600, 400, "webp")
		require.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("a file another variant reuses is never replaced with different settings", func(t *testing.T) {
		imageID := uuid.NewV4()
		source := host.upload(t, imageID, 1500, 1000)
		first := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{withQuality(webpSpec("600x600.webp", 600, 600), 90)}})
		host.waitForJob(t, imageID, first, StatusCompleted)
		stored, err := host.store.ReadVariant(t.Context(), testNamespace, imageID, 600, 400, "webp")
		require.NoError(t, err)

		// The first variant reuses the stored file, so the second one can't be stored over it at another quality
		second := host.enqueue(t, JobRequest{ImageID: imageID.String(), Variants: []VariantSpec{
			withQuality(webpSpec("600x600.webp", 600, 600), 90),
			withQuality(webpSpec("600xauto.webp", 600, 0), 20),
		}})
		status := host.waitForJob(t, imageID, second, StatusFailed)
		assert.Contains(t, status.Error, "reuses as stored with quality 90")

		// The stored file and the manifest describing it are what the first job left
		after, err := host.store.ReadVariant(t.Context(), testNamespace, imageID, 600, 400, "webp")
		require.NoError(t, err)
		assert.Equal(t, stored, after)
		m := host.manifest(t, imageID)
		assert.Equal(t, first, m.JobID)
	})

	t.Run("the same settings share the file", func(t *testing.T) {
		imageID := uuid.NewV4()
		source := host.upload(t, imageID, 1500, 1000)
		first := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{withQuality(webpSpec("600x600.webp", 600, 600), 90)}})
		host.waitForJob(t, imageID, first, StatusCompleted)
		initial := host.manifest(t, imageID)

		second := host.enqueue(t, JobRequest{ImageID: imageID.String(), Variants: []VariantSpec{
			withQuality(webpSpec("600x600.webp", 600, 600), 90),
			withQuality(webpSpec("600xauto.webp", 600, 0), 90),
		}})
		host.waitForJob(t, imageID, second, StatusCompleted)

		// Both entries describe the stored file, which the second variant kept rather than encoded again
		m := host.manifest(t, imageID)
		require.Len(t, m.Variants, 2)
		assert.Equal(t, m.Variants[0].Key, m.Variants[1].Key)
		assert.Equal(t, initial.Variants[0].Size, m.Variants[0].Size)
		assert.Equal(t, initial.Variants[0].Size, m.Variants[1].Size)
	})
}

// An upload that names an image ID holds it while it stores the image, so a second upload of the same ID can't store it at the same time
func TestImageIDClaims(t *testing.T) {
	host := startTestHost(t, testHostOptions{})
	jobs := host.Jobs()
	ctx := t.Context()

	claim := func(t *testing.T, imageID uuid.UUID, token string, ttl time.Duration) bool {
		t.Helper()
		claimed, err := jobs.ClaimUpload(ctx, testNamespace, imageID.String(), token, ttl)
		require.NoError(t, err)
		return claimed
	}
	release := func(t *testing.T, imageID uuid.UUID, token string) {
		t.Helper()
		err := jobs.ReleaseUpload(ctx, testNamespace, imageID.String(), token)
		require.NoError(t, err)
	}

	t.Run("one upload at a time", func(t *testing.T) {
		imageID := uuid.NewV4()
		require.True(t, claim(t, imageID, "first", time.Minute))
		assert.False(t, claim(t, imageID, "second", time.Minute))

		// The holder can claim again, and a release by another upload changes nothing
		assert.True(t, claim(t, imageID, "first", time.Minute))
		release(t, imageID, "second")
		assert.False(t, claim(t, imageID, "second", time.Minute))

		// Once released, the ID can be claimed again
		release(t, imageID, "first")
		assert.True(t, claim(t, imageID, "second", time.Minute))
		release(t, imageID, "second")
	})

	t.Run("a claim that is never released expires", func(t *testing.T) {
		imageID := uuid.NewV4()
		require.True(t, claim(t, imageID, "abandoned", 50*time.Millisecond))
		require.Eventually(t, func() bool {
			return claim(t, imageID, "next", time.Minute)
		}, 5*time.Second, 50*time.Millisecond)
		release(t, imageID, "next")
	})
}

// A job that is running or waiting means the image is in use, so an upload naming it is refused
func TestImageIDClaimsWaitForTheImageJobs(t *testing.T) {
	// The webhook holds the job open until the claim has been tried, so the job is still running when it arrives
	unblock := make(chan struct{})
	receiver := newTestReceiver(t, func(w http.ResponseWriter, _ webhook.Event) {
		<-unblock
		w.WriteHeader(http.StatusNoContent)
	})
	host := startTestHost(t, testHostOptions{webhookURL: receiver.URL})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 400, 300)
	jobID := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}, Notify: &webhook.Notify{Endpoint: "test"}})

	// Wait for the delivery, which the job makes after its manifest step
	select {
	case <-receiver.ids:
	case <-time.After(workflowWaitTimeout):
		t.Fatal("the job never delivered its event")
	}

	claimed, err := host.Jobs().ClaimUpload(t.Context(), testNamespace, imageID.String(), "upload", time.Minute)
	require.NoError(t, err)
	assert.False(t, claimed)

	close(unblock)
	host.waitForJob(t, imageID, jobID, StatusCompleted)
}

// An image an upload is still storing has no original yet, so a job that isn't the upload's own waits until the upload has queued it
func TestJobsWaitForAnUploadInProgress(t *testing.T) {
	host := startTestHost(t, testHostOptions{})
	jobs := host.Jobs()
	imageID := uuid.NewV4()

	claimed, err := jobs.ClaimUpload(t.Context(), testNamespace, imageID.String(), "upload", time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)

	// The original isn't stored yet, so an optimize job would only fail with image_not_found
	optimize := JobRequest{Namespace: testNamespace, ImageID: imageID.String(), SourceFormat: "png", Variants: []VariantSpec{webpSpec("200x200.webp", 200, 200)}, RequestedAt: time.Now()}
	_, err = jobs.Enqueue(t.Context(), optimize)
	require.ErrorIs(t, err, ErrUploadInProgress)

	// The upload's own job is accepted while it holds the claim, and a job sent after it waits behind it
	source := host.upload(t, imageID, 400, 300)
	uploadJob := host.enqueue(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec("100x100.webp", 100, 100)}})
	optimizeJob := host.enqueue(t, optimize)

	err = jobs.ReleaseUpload(t.Context(), testNamespace, imageID.String(), "upload")
	require.NoError(t, err)
	host.waitForJob(t, imageID, uploadJob, StatusCompleted)
	host.waitForJob(t, imageID, optimizeJob, StatusCompleted)

	// A claim that expired no longer holds anything back
	otherID := uuid.NewV4()
	claimed, err = jobs.ClaimUpload(t.Context(), testNamespace, otherID.String(), "abandoned", 50*time.Millisecond)
	require.NoError(t, err)
	require.True(t, claimed)
	time.Sleep(100 * time.Millisecond)
	optimize.ImageID = otherID.String()
	_, err = jobs.Enqueue(t.Context(), optimize)
	require.NoError(t, err)
}

func TestHostRateLimiter(t *testing.T) {
	host := startTestHost(t, testHostOptions{configure: func(c *config.Config) {
		c.RateLimit.Rate = 1
		c.RateLimit.Per = time.Minute
		c.RateLimit.Burst = 1
	}})

	allowed, retryAfter, err := host.RateLimiter().Allow(t.Context(), "198.51.100.7")
	require.NoError(t, err)
	assert.True(t, allowed)
	assert.Zero(t, retryAfter)

	allowed, retryAfter, err = host.RateLimiter().Allow(t.Context(), "198.51.100.7")
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Positive(t, retryAfter)
}

func TestJobServiceStartValidation(t *testing.T) {
	host := startTestHost(t, testHostOptions{})
	jobs := host.jobs.jobs

	tests := map[string]JobRequest{
		"an invalid namespace": {JobID: uuid.NewV7().String(), Namespace: "Not Valid", ImageID: uuid.NewV4().String()},
		"an invalid image ID":  {JobID: uuid.NewV7().String(), Namespace: testNamespace, ImageID: "../escape"},
		"a v4 job ID":          {JobID: uuid.NewV4().String(), Namespace: testNamespace, ImageID: uuid.NewV4().String()},
		"no job ID":            {Namespace: testNamespace, ImageID: uuid.NewV4().String()},
	}
	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			err := jobs.start(t.Context(), req)
			require.Error(t, err)
		})
	}

	// A job ID is only ever started once, so starting it again is a no-op rather than an error
	t.Run("starting the same job twice", func(t *testing.T) {
		imageID := uuid.NewV4()
		source := host.upload(t, imageID, 800, 600)
		req := JobRequest{JobID: uuid.NewV7().String(), Namespace: testNamespace, ImageID: imageID.String(), Source: &source}
		err := jobs.start(t.Context(), req)
		require.NoError(t, err)
		err = jobs.start(t.Context(), req)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			status, found, err := jobs.instanceStatus(t.Context(), testNamespace, imageID.String(), req.JobID)
			return err == nil && found && status.Status == francisworkflow.StatusCompleted
		}, workflowWaitTimeout, 100*time.Millisecond)
	})
}
