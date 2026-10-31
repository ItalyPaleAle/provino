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
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/italypaleale/francis/actor"
	francisworkflow "github.com/italypaleale/francis/builtin/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/imagekeys"
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

func webpSpec(width int, height int) VariantSpec {
	return VariantSpec{Format: imaging.FormatWebP, Quality: 80, Effort: 5, Resolution: imaging.Resolution{Width: width, Height: height}, Fit: imaging.FitContain}
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
			Key:    imagekeys.New().AssetKey(testNamespace, imageID, 100, 75, "0000000b", imaging.FormatWebP),
			Format: imaging.FormatWebP, Quality: 80, Resolution: imaging.Resolution{Width: 100, Height: 100},
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
		spec := webpSpec(200, 200)
		key := imageStore.AssetKey(testNamespace, imageID, 200, 150, "0000000a", spec.Format)
		err := imageStore.SaveVariant(t.Context(), key, []byte("new"), "image/webp")
		require.NoError(t, err)

		compensation := testCompensation{
			step:   stepGenerate,
			item:   task(spec),
			result: VariantResult{Key: key, Width: 200, Height: 150},
		}
		err = handlers.deleteVariant(t.Context(), compensation)
		require.NoError(t, err)
		err = handlers.deleteVariant(t.Context(), compensation)
		require.NoError(t, err)

		_, err = imageStore.ReadVariant(t.Context(), key)
		require.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("a reused variant is kept", func(t *testing.T) {
		spec := webpSpec(100, 100)
		key := imageStore.AssetKey(testNamespace, imageID, 100, 75, "0000000b", spec.Format)
		entry := manifest.Variant{Key: key, Format: imaging.FormatWebP, Width: 100, Height: 75, Size: 9}
		err := imageStore.SaveVariant(t.Context(), key, []byte("old"), "image/webp")
		require.NoError(t, err)

		err = handlers.deleteVariant(t.Context(), testCompensation{
			step:   stepGenerate,
			item:   task(spec),
			result: VariantResult{Width: 100, Height: 75, Reused: &entry},
		})
		require.NoError(t, err)

		content, err := imageStore.ReadVariant(t.Context(), key)
		require.NoError(t, err)
		assert.Equal(t, "old", string(content))
	})

	// The key comes from the journal, so one that isn't the image's own is never deleted
	t.Run("a key outside the image is refused", func(t *testing.T) {
		spec := webpSpec(300, 300)
		key := imageStore.AssetKey(testNamespace, uuid.NewV4(), 300, 225, "0000000c", spec.Format)
		err := imageStore.SaveVariant(t.Context(), key, []byte("another image's"), "image/webp")
		require.NoError(t, err)

		err = handlers.deleteVariant(t.Context(), testCompensation{
			step:   stepGenerate,
			item:   task(spec),
			result: VariantResult{Key: key, Width: 300, Height: 225},
		})
		require.ErrorIs(t, err, actor.ErrJobPermanentFailure)

		_, err = imageStore.ReadVariant(t.Context(), key)
		require.NoError(t, err)
	})

	t.Run("the manifest the job found is put back", func(t *testing.T) {
		previous, err := json.Marshal(previousManifest(imageID))
		require.NoError(t, err)
		err = imageStore.WriteManifest(t.Context(), testNamespace, imageID, map[string]string{"version": manifest.Version, "written": "by the failing job"})
		require.NoError(t, err)

		compensation := testCompensation{
			step:    stepManifest,
			input:   JobRequest{Namespace: testNamespace, ImageID: imageID.String(), Variants: []VariantSpec{webpSpec(100, 100)}},
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
			input:   JobRequest{Namespace: testNamespace, ImageID: imageID.String(), Variants: []VariantSpec{webpSpec(100, 100)}},
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
	box := func(width int, height int) imaging.Resolution {
		return imaging.Resolution{Width: width, Height: height}
	}
	key := func(width int, height int, fileID string) string {
		return imageStore.AssetKey(testNamespace, imageID, width, height, fileID, imaging.FormatWebP)
	}

	existing := manifest.Variant{Key: key(100, 75, "00000001"), Format: imaging.FormatWebP, Quality: 80, Resolution: box(100, 100), Fit: imaging.FitContain, Width: 100, Height: 75, Size: 9}
	missing := manifest.Variant{Key: key(200, 150, "00000002"), Format: imaging.FormatWebP, Quality: 80, Resolution: box(200, 200), Fit: imaging.FitContain, Width: 200, Height: 150, Size: 7}
	// A skipped entry repeats another entry's file, so it never stands for a file of its own
	skipped := manifest.Variant{Key: existing.Key, Format: imaging.FormatWebP, Quality: 80, Resolution: box(300, 300), Fit: imaging.FitContain, Width: 100, Height: 75, Size: 9, Skipped: true, SkippedFor: &imaging.Resolution{Width: 100, Height: 100}}
	// A file stored under an older layout's name is found by the key its entry records
	legacy := manifest.Variant{Key: imagekeys.New().Prefix(testNamespace, imageID) + imageID.String() + "_400x300.webp", Format: imaging.FormatWebP, Quality: 80, Resolution: box(400, 400), Fit: imaging.FitContain, Width: 400, Height: 300, Size: 11}
	// A key that names another image's file is never trusted, even when that file exists
	other := uuid.NewV4()
	foreign := manifest.Variant{Key: imageStore.AssetKey(testNamespace, other, 500, 375, "00000003", imaging.FormatWebP), Format: imaging.FormatWebP, Quality: 80, Resolution: box(500, 500), Fit: imaging.FitContain, Width: 500, Height: 375, Size: 13}
	for _, entry := range []manifest.Variant{existing, legacy, foreign} {
		err := imageStore.SaveVariant(t.Context(), entry.Key, []byte("variant"), entry.Format.ContentType())
		require.NoError(t, err)
	}
	previous := manifest.Manifest{Version: manifest.Version, Namespace: testNamespace, ImageID: imageID.String(), Status: manifest.StatusCompleted, SourceFormat: "png", Variants: []manifest.Variant{existing, missing, skipped, legacy, foreign}}

	source := imaging.Resolution{Width: 800, Height: 600}
	handlers := &jobWorkflowHandlers{runtime: &Runtime{Store: imageStore, Log: slog.New(slog.DiscardHandler)}}
	stored, err := handlers.reusableVariants(t.Context(), testNamespace, imageID, &previous)
	require.NoError(t, err)
	assert.Equal(t, map[variantIdentity]manifest.Variant{
		{Format: imaging.FormatWebP, Fit: imaging.FitContain, Quality: 80, Width: 100, Height: 100}: existing,
		{Format: imaging.FormatWebP, Fit: imaging.FitContain, Quality: 80, Width: 400, Height: 400}: legacy,
	}, stored.byBox)

	// The same box with another fit is another picture, so it doesn't reuse the file
	_, found := stored.lookup(source, VariantSpec{Format: imaging.FormatWebP, Quality: 80, Resolution: existing.Resolution, Fit: imaging.FitCover})
	assert.False(t, found)

	// So is the same box at another quality, which is what lets a site change its quality and get new files
	_, found = stored.lookup(source, VariantSpec{Format: imaging.FormatWebP, Quality: 60, Resolution: existing.Resolution, Fit: imaging.FitContain})
	assert.False(t, found)

	// A request that leaves the fit out means contain
	entry, found := stored.lookup(source, VariantSpec{Format: imaging.FormatWebP, Quality: 80, Resolution: existing.Resolution})
	assert.True(t, found)
	assert.Equal(t, existing, entry)

	result, err := handlers.generate(t.Context(), variantTask{
		Namespace: testNamespace,
		ImageID:   imageID.String(),
		Spec:      VariantSpec{Format: existing.Format, Quality: 80, Resolution: existing.Resolution},
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
		byBox:    map[variantIdentity]manifest.Variant{{Format: imaging.FormatWebP, Fit: imaging.FitContain, Quality: 80, Width: 1400, Height: 1400}: full},
		byOutput: map[variantIdentity]manifest.Variant{{Format: imaging.FormatWebP, Fit: imaging.FitContain, Quality: 80, Width: 1200, Height: 800}: full},
	}
	spec := func(format imaging.Format, quality int, side int) VariantSpec {
		return VariantSpec{Format: format, Quality: quality, Resolution: imaging.Resolution{Width: side, Height: side}, Fit: imaging.FitContain}
	}

	entry, found := stored.lookup(source, spec(imaging.FormatWebP, 80, 1300))
	assert.True(t, found)
	assert.Equal(t, full, entry)

	// A box the source fills needs a scaled file, which no other box's file is
	_, found = stored.lookup(source, spec(imaging.FormatWebP, 80, 1000))
	assert.False(t, found)

	// Another format or quality is another file
	_, found = stored.lookup(source, spec(imaging.FormatAVIF, 80, 1300))
	assert.False(t, found)
	_, found = stored.lookup(source, spec(imaging.FormatWebP, 60, 1300))
	assert.False(t, found)
}

func TestImageActorNextCheck(t *testing.T) {
	a := &imageActor{runtime: &Runtime{WorkflowTimeout: 10 * time.Minute, CompletedWorkflowRetention: 24 * time.Hour}}
	now := time.Now()

	// A running job is looked at every minute rather than only after its hour-long deadline, so a lost release doesn't hold the image that long
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

	// The oldest finished job expires with its journal
	finishedAt := now.Add(-time.Hour)
	due = a.nextCheck(&imageState{History: []finishedJob{{FinishedAt: finishedAt}}}, now)
	assert.Equal(t, finishedAt.Add(a.runtime.CompletedWorkflowRetention), due)

	// A history kept indefinitely has nothing that expires, so nothing is due
	a.runtime.CompletedWorkflowRetention = 0
	due = a.nextCheck(&imageState{History: []finishedJob{{FinishedAt: finishedAt}}}, now)
	assert.True(t, due.IsZero())
}

// fakeImageClient keeps an image actor's state and alarm in memory, and counts the writes the actor makes
type fakeImageClient struct {
	actor.Client[imageState]

	state  imageState
	alarm  time.Time
	writes int
}

func (c *fakeImageClient) GetState(context.Context) (imageState, error) {
	return c.state, nil
}

func (c *fakeImageClient) SetState(_ context.Context, state imageState, _ *actor.SetStateOpts) error {
	c.writes++
	c.state = state
	return nil
}

func (c *fakeImageClient) DeleteState(context.Context) error {
	c.writes++
	c.state = imageState{}
	return nil
}

func (c *fakeImageClient) SetAlarm(_ context.Context, _ string, properties actor.AlarmProperties) error {
	c.writes++
	c.alarm = properties.DueTime
	return nil
}

func (c *fakeImageClient) DeleteAlarm(context.Context, string) error {
	c.writes++
	c.alarm = time.Time{}
	return nil
}

// A refused start or claim leaves the state as it found it, so a caller retrying against a busy image costs no writes
func TestImageActorRefusalsWriteNothing(t *testing.T) {
	client := &fakeImageClient{state: imageState{Upload: &uploadClaim{Token: "upload", ExpiresAt: time.Now().Add(time.Minute)}}}
	a := &imageActor{namespace: testNamespace, imageID: uuid.NewV4().String(), client: client, runtime: &Runtime{Log: slog.New(slog.DiscardHandler)}}

	started, err := a.startJob(t.Context(), JobRequest{Namespace: testNamespace, ImageID: a.imageID, SourceFormat: "png"})
	require.NoError(t, err)
	assert.True(t, started.Busy)

	claimed, err := a.claim(t.Context(), claimRequest{Token: "another upload", TTL: time.Minute})
	require.NoError(t, err)
	assert.False(t, claimed.Claimed)

	assert.Zero(t, client.writes)
}

// A history kept indefinitely is kept without an alarm, rather than with one that fires every day for nothing
func TestImageActorKeepsAnIndefiniteHistoryWithoutAnAlarm(t *testing.T) {
	client := &fakeImageClient{alarm: time.Now()}
	a := &imageActor{client: client, runtime: &Runtime{}}

	err := a.save(t.Context(), &imageState{History: []finishedJob{{JobID: uuid.NewV7().String(), FinishedAt: time.Now()}}})
	require.NoError(t, err)
	assert.Len(t, client.state.History, 1)
	assert.True(t, client.alarm.IsZero())
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

	source := JobSource{Format: "png", Size: size, Width: width, Height: height, StoredAt: time.Now().UTC().Truncate(time.Second)}
	err = h.store.WriteManifest(t.Context(), testNamespace, imageID, manifest.Manifest{
		Version: manifest.Version, Namespace: testNamespace, ImageID: imageID.String(), Status: manifest.StatusStored,
		SourceKey: h.store.OriginalKey(testNamespace, imageID, "png"), SourceFormat: "png", SourceSize: size,
		SourceWidth: width, SourceHeight: height, StoredAt: source.StoredAt, Variants: []manifest.Variant{},
	})
	require.NoError(t, err)

	return source
}

// Start a job and return its ID
func (h *testHost) startJob(t *testing.T, req JobRequest) string {
	t.Helper()

	if req.Namespace == "" {
		req.Namespace = testNamespace
	}
	if req.RequestedAt.IsZero() {
		req.RequestedAt = time.Now()
	}
	jobID, err := h.Jobs().Start(t.Context(), req)
	require.NoError(t, err)
	return jobID
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

// Check that each time is set and in UTC, which is how provino records and reports every time whatever the local zone
func assertUTC(t *testing.T, times ...time.Time) {
	t.Helper()

	for i, ts := range times {
		assert.False(t, ts.IsZero(), "time %d isn't set", i)
		assert.Equal(t, time.UTC, ts.Location(), "time %d (%s) isn't in UTC", i, ts)
	}
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
		webpSpec(400, 400),
		{Format: imaging.FormatAVIF, Quality: 40, Effort: 4, Resolution: imaging.Resolution{Width: 200, Height: 200}, Fit: imaging.FitCover},
		{Format: imaging.FormatJPEG, Quality: 85, Resolution: imaging.Resolution{Width: 800}, Fit: imaging.FitContain},
	}
	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: variants, Notify: &webhook.Notify{Endpoint: "test", Context: json.RawMessage(`{"site":"s1"}`)}})

	event := receiver.next(t)
	assert.Equal(t, webhook.EventImageOptimized, event.Type)
	assert.Equal(t, jobID, event.Data.JobID)
	assert.JSONEq(t, `{"site":"s1"}`, string(event.Data.Context))
	assert.Equal(t, host.store.ManifestKey(testNamespace, imageID), event.Data.ManifestKey)
	assert.Equal(t, jobID+".image.optimized", <-receiver.ids)

	status := host.waitForJob(t, imageID, jobID, StatusCompleted)
	assert.Equal(t, 3, status.Total)
	assert.Equal(t, 3, status.Completed)

	// The request reaches the workflow through the image actor, whose state Francis decodes in the local zone, and the times still come out in UTC
	m := host.manifest(t, imageID)
	assert.Equal(t, manifest.StatusCompleted, m.Status)
	assert.Equal(t, jobID, m.JobID)
	assert.Equal(t, source.Size, m.SourceSize)
	assert.Equal(t, source.StoredAt, m.StoredAt)
	assertUTC(t, m.StoredAt, m.RequestedAt, m.StartedAt, m.CompletedAt, event.Timestamp)
	assertUTC(t, status.RequestedAt, status.StartedAt, status.CompletedAt)
	require.Len(t, m.Variants, len(variants))
	require.NotNil(t, event.Data.Manifest)
	assert.Equal(t, m.Variants, event.Data.Manifest.Variants)

	for i, entry := range m.Variants {
		// The manifest lists the variants in the order the job asked for them
		assert.Equal(t, variants[i].Resolution, entry.Resolution)
		assert.Equal(t, variants[i].Format, entry.Format)
		assertVariantKey(t, imageID, entry)

		// Every key in the manifest resolves to a stored file of the recorded size
		content, err := host.store.ReadVariant(t.Context(), entry.Key)
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

	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Notify: &webhook.Notify{Endpoint: "test"}})

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
	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{
		webpSpec(100, 100),
		webpSpec(200, 200),
		webpSpec(300, 300),
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

	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Variants: []VariantSpec{webpSpec(100, 100)}, Notify: &webhook.Notify{Endpoint: "test"}})

	event := receiver.next(t)
	assert.Equal(t, webhook.EventImageFailed, event.Type)
	assert.NotEmpty(t, event.Data.Error)
	require.NotNil(t, event.Data.Manifest)
	assert.Equal(t, manifest.StatusStored, event.Data.Manifest.Status)

	status := host.waitForJob(t, imageID, jobID, StatusFailed)
	assert.NotEmpty(t, status.Error)

	// Nothing the job stored is left, and the manifest is exactly what it was
	assert.Empty(t, storedVariantKeys(t, imageID))
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
	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), SourceFormat: "jpg", RequestedAt: requestedAt, Variants: []VariantSpec{webpSpec(100, 100)}, Notify: &webhook.Notify{Endpoint: "test"}})
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
	assertUTC(t, received...)
	assert.False(t, received[0].Before(start.Truncate(time.Second)), "the event is dated %s, before the job ran", received[0])
}

// A job for an image that doesn't exist fails before it writes anything, and says why
func TestJobForAMissingOriginalFails(t *testing.T) {
	receiver := newTestReceiver(t, nil)
	host := startTestHost(t, testHostOptions{webhookURL: receiver.URL})

	imageID := uuid.NewV4()
	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), SourceFormat: "jpg", Variants: []VariantSpec{webpSpec(100, 100)}, Notify: &webhook.Notify{Endpoint: "test"}})

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
	otherJob := host.startJob(t, JobRequest{ImageID: otherID.String(), Variants: []VariantSpec{webpSpec(100, 100)}})
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
	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec(100, 100)}, Notify: &webhook.Notify{Endpoint: "test"}})

	// The variants and the manifest are there for the application to find, so the job completes and the undelivered event is only recorded
	host.waitForJob(t, imageID, jobID, StatusCompleted)
	assert.Equal(t, manifest.StatusCompleted, host.manifest(t, imageID).Status)

	// Delivery is retried before the optional step is given up on, and every attempt carries the same delivery ID
	assert.Greater(t, attempts.Load(), int32(1), "the event must be retried before the job gives up on it")
	first := <-receiver.ids
	second := <-receiver.ids
	assert.Equal(t, first, second)
}

// Check that a manifest entry's key names the image, the size the file came out at, a file ID and the format
func assertVariantKey(t *testing.T, imageID uuid.UUID, entry manifest.Variant) {
	t.Helper()

	fileID := imagekeys.FileIDOf(entry.Key)
	require.NotEmpty(t, fileID, entry.Key)
	assert.Equal(t, imagekeys.New().AssetKey(testNamespace, imageID, entry.Width, entry.Height, fileID, entry.Format), entry.Key)
}

// List the keys of the variant files stored for an image, which the file system store keeps as files under the image's prefix
func storedVariantKeys(t *testing.T, imageID uuid.UUID) []string {
	t.Helper()

	layout := imagekeys.New()
	prefix := layout.Prefix(testNamespace, imageID)
	entries, err := os.ReadDir(filepath.Join(config.Get().Storage.Variants, filepath.FromSlash(prefix)))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)

	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		key := prefix + e.Name()
		if !strings.HasPrefix(e.Name(), imageID.String()+"_") || key == layout.ManifestKey(testNamespace, imageID) {
			continue
		}
		keys = append(keys, key)
	}
	return keys
}

// Keep the jobs of an image running until the returned function is called
// The image's manifest is replaced with a folder, which the snapshot step fails to read with an error worth retrying, so the job stays in its first step without ever failing
// The job has to carry what the upload measured, since the manifest it would read that from is gone
func holdJobs(t *testing.T, imageID uuid.UUID) (resume func()) {
	t.Helper()

	path := filepath.Join(config.Get().Storage.Variants, filepath.FromSlash(imagekeys.New().ManifestKey(testNamespace, imageID)))
	err := os.Remove(path)
	require.NoError(t, err)
	err = os.Mkdir(path, 0o750)
	require.NoError(t, err)

	// Francis retries the step after 2 and then 4 more seconds, so the job has to be resumed within about 6 seconds or it fails
	return func() {
		err := os.Remove(path)
		require.NoError(t, err)
	}
}

// The jobs of one image run one at a time, so a job sent while another is running is refused, and can be sent again once that one has finished
func TestJobsOfOneImageRunOneAtATime(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	resume := holdJobs(t, imageID)
	first := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec(100, 100)}})

	// The image is busy, so a second job is refused rather than kept for later
	second := JobRequest{Namespace: testNamespace, ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec(200, 200)}, RequestedAt: time.Now()}
	_, err := host.Jobs().Start(t.Context(), second)
	require.ErrorIs(t, err, ErrImageBusy)

	// The running job reports as running, and an image that isn't this one doesn't know the job at all
	status, _, found, err := host.Jobs().Status(t.Context(), testNamespace, imageID.String(), first)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, StatusRunning, status.Status)
	_, _, found, err = host.Jobs().Status(t.Context(), testNamespace, uuid.NewV4().String(), first)
	require.NoError(t, err)
	assert.False(t, found)

	// Once the first job has finished, the second one is accepted
	resume()
	host.waitForJob(t, imageID, first, StatusCompleted)
	secondID := host.startJob(t, second)
	host.waitForJob(t, imageID, secondID, StatusCompleted)

	m := host.manifest(t, imageID)
	assert.Equal(t, secondID, m.JobID)
	require.Len(t, m.Variants, 1)
	assert.Equal(t, imaging.Resolution{Width: 200, Height: 200}, m.Variants[0].Resolution)
}

// A job frees its image before it delivers its event, so a slow or failing receiver doesn't keep the image busy
func TestTheImageIsFreeWhileTheEventIsDelivered(t *testing.T) {
	// Every delivery is held until the second job has run, and only the first job has an endpoint to deliver to
	release := make(chan struct{})
	var held atomic.Bool
	receiver := newTestReceiver(t, func(w http.ResponseWriter, _ webhook.Event) {
		held.Store(true)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	var releaseOnce sync.Once
	unblock := func() {
		releaseOnce.Do(func() { close(release) })
	}
	t.Cleanup(unblock)
	host := startTestHost(t, testHostOptions{webhookURL: receiver.URL})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	first := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec(100, 100)}, Notify: &webhook.Notify{Endpoint: "test"}})
	require.Eventually(t, held.Load, workflowWaitTimeout, 50*time.Millisecond, "the first job did not reach its delivery")

	// The first job is still delivering its event, and the image already takes the next job
	second := host.startJob(t, JobRequest{ImageID: imageID.String(), Variants: []VariantSpec{webpSpec(200, 200)}})
	host.waitForJob(t, imageID, second, StatusCompleted)
	status, _, found, err := host.Jobs().Status(t.Context(), testNamespace, imageID.String(), first)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, StatusRunning, status.Status)

	unblock()
	host.waitForJob(t, imageID, first, StatusCompleted)
	assert.Equal(t, second, host.manifest(t, imageID).JobID)
}

// A job's status says who started it, which is what lets a browser read its own jobs and nobody else's
func TestJobStatusCarriesTheSubject(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, SubjectHash: "hash-of-the-subject"})
	host.waitForJob(t, imageID, jobID, StatusCompleted)

	_, subjectHash, found, err := host.Jobs().Status(t.Context(), testNamespace, imageID.String(), jobID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "hash-of-the-subject", subjectHash)

	_, _, found, err = host.Jobs().Status(t.Context(), testNamespace, imageID.String(), uuid.NewV7().String())
	require.NoError(t, err)
	assert.False(t, found)
}

// An active job whose workflow doesn't exist, which a crash between recording and starting it leaves behind, doesn't hold the image
func TestImageActorFreesAStaleActiveJob(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	err := host.host.SetState(t.Context(), imageActorType, imageActorID(testNamespace, imageID.String()), imageState{
		Active: &activeJob{JobID: uuid.NewV7().String(), StartedAt: time.Now()},
	}, nil)
	require.NoError(t, err)

	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec(100, 100)}})
	host.waitForJob(t, imageID, jobID, StatusCompleted)
}

// A process that stops after recording a job as active and before starting its workflow leaves the alarm scheduled first, which starts the job rather than dropping it
func TestImageActorStartsAnActiveJobThatNeverStarted(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	jobID := uuid.NewV7().String()
	request := JobRequest{JobID: jobID, Namespace: testNamespace, ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec(100, 100)}, RequestedAt: time.Now()}

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
	request := JobRequest{JobID: staleID, Namespace: testNamespace, ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec(100, 100)}, RequestedAt: time.Now()}

	// The job became active two hours ago, when the retention was an hour, and its start was never confirmed
	err := host.host.SetState(t.Context(), imageActorType, imageActorID(testNamespace, imageID.String()), imageState{
		Active: &activeJob{JobID: staleID, StartedAt: time.Now().Add(-2 * time.Hour), Request: &request, Retention: time.Hour},
	}, nil)
	require.NoError(t, err)

	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Variants: []VariantSpec{webpSpec(200, 200)}})
	host.waitForJob(t, imageID, jobID, StatusCompleted)

	// The stale job never ran, so it has no status and the manifest is the new job's
	_, _, found, err := host.Jobs().Status(t.Context(), testNamespace, imageID.String(), staleID)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, jobID, host.manifest(t, imageID).JobID)
}

// A start that fails may have been recorded by Francis anyway, so its job stays active without the request heal would start it from: a workflow that exists keeps the image busy, and one that doesn't is freed rather than started
func TestImageActorKeepsAJobWhoseStartFailed(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)

	// A context larger than a workflow's input can hold makes the start fail
	oversized := JobRequest{Namespace: testNamespace, ImageID: imageID.String(), Source: &source, Notify: &webhook.Notify{Context: json.RawMessage(`{"a":"` + strings.Repeat("a", 70<<10) + `"}`)}, RequestedAt: time.Now()}
	_, err := host.Jobs().Start(t.Context(), oversized)
	require.Error(t, err)

	var state imageState
	err = host.host.Service().GetState(t.Context(), imageActorType, imageActorID(testNamespace, imageID.String()), &state)
	require.NoError(t, err)
	require.NotNil(t, state.Active)
	assert.Nil(t, state.Active.Request)
	failedID := state.Active.JobID

	// This start never reached Francis, so the next job frees the image and runs, and the failed one never does
	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Variants: []VariantSpec{webpSpec(100, 100)}})
	host.waitForJob(t, imageID, jobID, StatusCompleted)
	_, found, err := host.jobs.jobs.instanceStatus(t.Context(), testNamespace, imageID.String(), failedID)
	require.NoError(t, err)
	assert.False(t, found)
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
	first := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec(100, 100)}})
	host.waitForJob(t, imageID, first, StatusCompleted)
	initial := host.manifest(t, imageID)
	require.Len(t, initial.Variants, 1)

	// The effort isn't part of what a variant is, so a stored file is reused whatever effort is asked for
	// An invalid one proves the existing pair is never passed to the encoder while the new pair still needs the source
	invalid := webpSpec(100, 100)
	invalid.Effort = 99
	second := host.startJob(t, JobRequest{ImageID: imageID.String(), Variants: []VariantSpec{invalid, webpSpec(200, 200)}})
	host.waitForJob(t, imageID, second, StatusCompleted)

	regenerated := host.manifest(t, imageID)
	require.Len(t, regenerated.Variants, 2)
	assert.Equal(t, 5, regenerated.Variants[0].Effort, "reused metadata must describe the stored encoding")
	assert.Equal(t, initial.Variants[0].Key, regenerated.Variants[0].Key)
	assert.Equal(t, initial.Variants[0].Size, regenerated.Variants[0].Size)
	assert.Equal(t, 200, regenerated.Variants[1].Width)
	assert.Equal(t, 150, regenerated.Variants[1].Height)
	// The original's details came from the manifest the upload wrote
	assert.Equal(t, initial.SourceSize, regenerated.SourceSize)
	assert.Equal(t, initial.StoredAt, regenerated.StoredAt)
}

// Changing the quality of a variant encodes it again into a file of its own, and the file encoded at the old quality stays for whoever still references it
func TestRegenerationAtAnotherQualityEncodesANewFile(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 600)
	first := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{withQuality(webpSpec(400, 400), 90)}})
	host.waitForJob(t, imageID, first, StatusCompleted)
	initial := host.manifest(t, imageID)
	before, err := host.store.ReadVariant(t.Context(), initial.Variants[0].Key)
	require.NoError(t, err)

	second := host.startJob(t, JobRequest{ImageID: imageID.String(), Variants: []VariantSpec{withQuality(webpSpec(400, 400), 20)}})
	host.waitForJob(t, imageID, second, StatusCompleted)

	m := host.manifest(t, imageID)
	require.Len(t, m.Variants, 1)
	assert.Equal(t, 20, m.Variants[0].Quality)
	assert.NotEqual(t, initial.Variants[0].Key, m.Variants[0].Key)
	_, err = host.store.ReadVariant(t.Context(), m.Variants[0].Key)
	require.NoError(t, err)

	after, err := host.store.ReadVariant(t.Context(), initial.Variants[0].Key)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// No variant is larger than its source, and boxes that would only repeat the source's pixels store nothing
func TestJobNeverEnlargesSmallOriginals(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 800)
	variants := []VariantSpec{
		webpSpec(500, 500),
		webpSpec(1000, 1000),
		webpSpec(2000, 2000),
		{Format: imaging.FormatWebP, Quality: 80, Effort: 5, Resolution: imaging.Resolution{Width: 1200, Height: 500}, Fit: imaging.FitCover},
		{Format: imaging.FormatWebP, Quality: 80, Effort: 5, Resolution: imaging.Resolution{Width: 2400, Height: 1000}, Fit: imaging.FitCover},
	}
	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: variants})
	host.waitForJob(t, imageID, jobID, StatusCompleted)

	m := host.manifest(t, imageID)
	require.Len(t, m.Variants, len(variants))

	// A box the source fills is scaled down as before
	assert.Equal(t, 500, m.Variants[0].Width)
	assert.False(t, m.Variants[0].Skipped)

	// The tightest box the source can't fill holds the source at its own size, which its key names, and the looser box repeats its file
	holder := m.Variants[1]
	assert.Equal(t, 800, holder.Width)
	assert.Equal(t, 800, holder.Height)
	assertVariantKey(t, imageID, holder)
	skipped := m.Variants[2]
	assert.True(t, skipped.Skipped)
	assert.Equal(t, &imaging.Resolution{Width: 1000, Height: 1000}, skipped.SkippedFor)
	assert.Equal(t, holder.Key, skipped.Key)

	// A cover box keeps its shape at the source's full width, and its larger twin is skipped
	assert.Equal(t, 800, m.Variants[3].Width)
	assert.Equal(t, 333, m.Variants[3].Height)
	assert.True(t, m.Variants[4].Skipped)
	assert.Equal(t, m.Variants[3].Key, m.Variants[4].Key)

	// The skipped boxes stored nothing, so the image has one file per box that holds one
	assert.ElementsMatch(t, []string{m.Variants[0].Key, holder.Key, m.Variants[3].Key}, storedVariantKeys(t, imageID))
}

// A file enlarged before provino stopped doing that is replaced by one at the source's own size
func TestRegenerationReplacesEnlargedFiles(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 800, 800)

	// A manifest listing a 1000x1000 file enlarged from the 800x800 source, stored under the name the older layout gave it
	enlargedKey := imagekeys.New().Prefix(testNamespace, imageID) + imageID.String() + "_1000x1000.webp"
	err := host.store.SaveVariant(t.Context(), enlargedKey, []byte("enlarged"), "image/webp")
	require.NoError(t, err)
	err = host.store.WriteManifest(t.Context(), testNamespace, imageID, manifest.Manifest{
		Version: manifest.Version, Namespace: testNamespace, ImageID: imageID.String(), Status: manifest.StatusCompleted,
		SourceFormat: "png", SourceWidth: 800, SourceHeight: 800,
		Variants: []manifest.Variant{{Key: enlargedKey, Format: imaging.FormatWebP, Quality: 80, Effort: 5, Resolution: imaging.Resolution{Width: 1000, Height: 1000}, Fit: imaging.FitContain, Width: 1000, Height: 1000, Size: int64(len("enlarged"))}},
	})
	require.NoError(t, err)

	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec(1000, 1000), webpSpec(2000, 2000)}})
	host.waitForJob(t, imageID, jobID, StatusCompleted)

	m := host.manifest(t, imageID)
	require.Len(t, m.Variants, 2)
	assert.Equal(t, 800, m.Variants[0].Width)
	assert.NotEqual(t, enlargedKey, m.Variants[0].Key, "the enlarged file must not be reused")
	assert.True(t, m.Variants[1].Skipped)
}

// Change the quality of a variant
func withQuality(spec VariantSpec, quality int) VariantSpec {
	spec.Quality = quality
	return spec
}

// Boxes that differ can come out at the same size for one source, and each of them is a file of its own
func TestBoxesThatComeOutAtOneSizeAreSeparateFiles(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	t.Run("each box keeps its own settings", func(t *testing.T) {
		imageID := uuid.NewV4()
		source := host.upload(t, imageID, 1500, 1000)

		// For a 3:2 source, a 600x600 box and a box 600 pixels wide both come out at 600x400
		jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{
			withQuality(webpSpec(600, 600), 90),
			withQuality(webpSpec(600, 0), 20),
		}})
		host.waitForJob(t, imageID, jobID, StatusCompleted)

		m := host.manifest(t, imageID)
		require.Len(t, m.Variants, 2)
		assert.NotEqual(t, m.Variants[0].Key, m.Variants[1].Key)
		assert.Equal(t, 90, m.Variants[0].Quality)
		assert.Equal(t, 20, m.Variants[1].Quality)
		for _, entry := range m.Variants {
			assert.Equal(t, 600, entry.Width)
			assert.Equal(t, 400, entry.Height)
			content, err := host.store.ReadVariant(t.Context(), entry.Key)
			require.NoError(t, err)
			assert.Equal(t, entry.Size, int64(len(content)))
		}
	})

	// One box can be asked for at several qualities, and each is a file of its own that a later job reuses by its quality
	t.Run("one box at two qualities", func(t *testing.T) {
		imageID := uuid.NewV4()
		source := host.upload(t, imageID, 1500, 1000)
		variants := []VariantSpec{
			withQuality(webpSpec(600, 600), 90),
			withQuality(webpSpec(600, 600), 40),
		}
		first := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: variants})
		host.waitForJob(t, imageID, first, StatusCompleted)

		m := host.manifest(t, imageID)
		require.Len(t, m.Variants, 2)
		assert.Equal(t, 90, m.Variants[0].Quality)
		assert.Equal(t, 40, m.Variants[1].Quality)
		assert.NotEqual(t, m.Variants[0].Key, m.Variants[1].Key)
		assert.ElementsMatch(t, []string{m.Variants[0].Key, m.Variants[1].Key}, storedVariantKeys(t, imageID))

		second := host.startJob(t, JobRequest{ImageID: imageID.String(), Variants: variants})
		host.waitForJob(t, imageID, second, StatusCompleted)
		again := host.manifest(t, imageID)
		require.Len(t, again.Variants, 2)
		assert.Equal(t, m.Variants[0].Key, again.Variants[0].Key)
		assert.Equal(t, m.Variants[1].Key, again.Variants[1].Key)
	})

	// A failed job puts back the manifest it found, and every file that manifest lists still holds the bytes it describes
	t.Run("a failed job never changes a file the manifest it found lists", func(t *testing.T) {
		imageID := uuid.NewV4()
		source := host.upload(t, imageID, 1500, 1000)
		first := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{withQuality(webpSpec(600, 600), 90)}})
		host.waitForJob(t, imageID, first, StatusCompleted)
		initial := host.manifest(t, imageID)
		stored, err := host.store.ReadVariant(t.Context(), initial.Variants[0].Key)
		require.NoError(t, err)

		// The second job encodes boxes that come out at the size of the stored file, and then fails on a quality the encoder refuses
		second := host.startJob(t, JobRequest{ImageID: imageID.String(), Variants: []VariantSpec{
			withQuality(webpSpec(600, 0), 20),
			withQuality(webpSpec(600, 500), 30),
			withQuality(webpSpec(300, 300), 101),
		}})
		host.waitForJob(t, imageID, second, StatusFailed)

		m := host.manifest(t, imageID)
		assert.Equal(t, first, m.JobID)
		after, err := host.store.ReadVariant(t.Context(), initial.Variants[0].Key)
		require.NoError(t, err)
		assert.Equal(t, stored, after)
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

// A running job means the image is in use, so an upload naming it is refused
func TestImageIDClaimsWaitForTheImageJobs(t *testing.T) {
	host := startTestHost(t, testHostOptions{})

	imageID := uuid.NewV4()
	source := host.upload(t, imageID, 400, 300)
	resume := holdJobs(t, imageID)
	jobID := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec(100, 100)}})

	claimed, err := host.Jobs().ClaimUpload(t.Context(), testNamespace, imageID.String(), "upload", time.Minute)
	require.NoError(t, err)
	assert.False(t, claimed)

	resume()
	host.waitForJob(t, imageID, jobID, StatusCompleted)
}

// An image an upload is still storing has no original yet, so the only job that may start is the upload's own, and any other is refused until the upload is done
func TestJobsWaitForAnUploadInProgress(t *testing.T) {
	host := startTestHost(t, testHostOptions{})
	jobs := host.Jobs()
	imageID := uuid.NewV4()

	claimed, err := jobs.ClaimUpload(t.Context(), testNamespace, imageID.String(), "upload", time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)

	// The original isn't stored yet, so an optimize job would only fail with image_not_found
	optimize := JobRequest{Namespace: testNamespace, ImageID: imageID.String(), SourceFormat: "png", Variants: []VariantSpec{webpSpec(200, 200)}, RequestedAt: time.Now()}
	_, err = jobs.Start(t.Context(), optimize)
	require.ErrorIs(t, err, ErrImageBusy)

	// The upload's own job starts while it holds the claim, and any other job is still refused until the upload has released it and its job has finished
	source := host.upload(t, imageID, 400, 300)
	uploadJob := host.startJob(t, JobRequest{ImageID: imageID.String(), Source: &source, Variants: []VariantSpec{webpSpec(100, 100)}})
	_, err = jobs.Start(t.Context(), optimize)
	require.ErrorIs(t, err, ErrImageBusy)

	err = jobs.ReleaseUpload(t.Context(), testNamespace, imageID.String(), "upload")
	require.NoError(t, err)
	host.waitForJob(t, imageID, uploadJob, StatusCompleted)
	optimizeJob := host.startJob(t, optimize)
	host.waitForJob(t, imageID, optimizeJob, StatusCompleted)

	// A claim that expired no longer holds anything back
	otherID := uuid.NewV4()
	claimed, err = jobs.ClaimUpload(t.Context(), testNamespace, otherID.String(), "abandoned", 50*time.Millisecond)
	require.NoError(t, err)
	require.True(t, claimed)
	time.Sleep(100 * time.Millisecond)
	optimize.ImageID = otherID.String()
	_, err = jobs.Start(t.Context(), optimize)
	require.NoError(t, err)
}

func TestHostRateLimiter(t *testing.T) {
	host := startTestHost(t, testHostOptions{configure: func(c *config.Config) {
		c.RateLimit.Rate = 1
		c.RateLimit.Per = time.Minute
		c.RateLimit.Burst = 1
	}})

	allowed, retryAfter, err := host.IngestRateLimiter().Allow(t.Context(), "198.51.100.7")
	require.NoError(t, err)
	assert.True(t, allowed)
	assert.Zero(t, retryAfter)

	allowed, retryAfter, err = host.IngestRateLimiter().Allow(t.Context(), "198.51.100.7")
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Positive(t, retryAfter)

	// Status lookups have a budget of their own, which the uploads didn't spend
	allowed, _, err = host.StatusRateLimiter().Allow(t.Context(), "198.51.100.7")
	require.NoError(t, err)
	assert.True(t, allowed)
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
