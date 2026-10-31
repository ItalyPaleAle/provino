package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/italypaleale/provino/manifest"
	"github.com/italypaleale/provino/webhook"
)

// Number of authorizer requests and webhook deliveries listed, which is enough to follow a test session
const listLimit = 100

// Outcomes of a webhook delivery, which the web UI shows
const (
	outcomeApplied = "applied"
	outcomeRepeat  = "repeat"
	outcomeStale   = "stale"
	outcomeIgnored = "ignored"
)

// Tables of the app's database
// Times are Unix nanoseconds, so they sort and compare as numbers
const schema = `
CREATE TABLE IF NOT EXISTS settings (
	name TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS images (
	namespace TEXT NOT NULL,
	image_id TEXT NOT NULL,
	owner TEXT NOT NULL,
	profile TEXT NOT NULL,
	status TEXT NOT NULL,
	job_id TEXT NOT NULL,
	error TEXT NOT NULL,
	manifest TEXT,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (namespace, image_id)
);
CREATE INDEX IF NOT EXISTS images_owner ON images (owner, updated_at);

CREATE TABLE IF NOT EXISTS authorizations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	received_at INTEGER NOT NULL,
	subject TEXT NOT NULL,
	request_headers TEXT NOT NULL,
	request_body TEXT NOT NULL,
	status INTEGER NOT NULL,
	response_body TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS authorizations_subject ON authorizations (subject, id);

CREATE TABLE IF NOT EXISTS deliveries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	received_at INTEGER NOT NULL,
	webhook_id TEXT NOT NULL,
	event_type TEXT NOT NULL,
	namespace TEXT NOT NULL,
	image_id TEXT NOT NULL,
	job_id TEXT NOT NULL,
	event_timestamp INTEGER NOT NULL,
	request_headers TEXT NOT NULL,
	request_body TEXT NOT NULL,
	status INTEGER NOT NULL,
	outcome TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS deliveries_namespace ON deliveries (namespace, id);
CREATE INDEX IF NOT EXISTS deliveries_webhook_id ON deliveries (webhook_id);
`

// db is the app's SQLite database, which holds everything it keeps
type db struct {
	conn *sql.DB
}

// imageRecord is what the app knows about one image, from the latest event about it
type imageRecord struct {
	Namespace string `json:"namespace"`
	ImageID   string `json:"imageId"`
	// Account that uploaded the image, and the profile it picked, from the context the authorizer asked provino to echo
	Owner   string `json:"owner"`
	Profile string `json:"profile,omitempty"`
	// Type of the latest event, which says whether the latest job completed
	Status webhook.EventType `json:"status"`
	JobID  string            `json:"jobId"`
	// Why the latest job failed
	Error string `json:"error,omitempty"`
	// The manifest the image has: the one the latest job wrote, or after a failure, the one the image was left with
	Manifest *manifest.Manifest `json:"manifest,omitempty"`
	// When the latest event happened, which is what orders events that arrive out of order
	UpdatedAt time.Time `json:"updatedAt"`
}

// authorizationRecord is one request provino made to the authorizer, with the app's answer
type authorizationRecord struct {
	ID             int64             `json:"id"`
	ReceivedAt     time.Time         `json:"receivedAt"`
	Subject        string            `json:"subject"`
	RequestHeaders map[string]string `json:"requestHeaders"`
	Request        json.RawMessage   `json:"request"`
	Status         int               `json:"status"`
	Response       json.RawMessage   `json:"response"`
}

// deliveryRecord is one webhook delivery provino made, with what the app did with it
type deliveryRecord struct {
	ID             int64             `json:"id"`
	ReceivedAt     time.Time         `json:"receivedAt"`
	WebhookID      string            `json:"webhookId"`
	Type           webhook.EventType `json:"type"`
	Namespace      string            `json:"namespace"`
	ImageID        string            `json:"imageId"`
	JobID          string            `json:"jobId"`
	Timestamp      time.Time         `json:"timestamp"`
	RequestHeaders map[string]string `json:"requestHeaders"`
	Body           json.RawMessage   `json:"body"`
	Status         int               `json:"status"`
	// What recording it did: applied, a repeat of a delivery already received, stale because a newer event was already applied, or ignored
	Outcome string `json:"outcome"`
}

// Open the database at path, creating it and its tables when they don't exist
func openDB(ctx context.Context, path string) (*db, error) {
	conn, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("failed to open the database: %w", err)
	}

	// One connection is enough for a test app, and it keeps SQLite's writes from ever contending
	conn.SetMaxOpenConns(1)

	_, err = conn.ExecContext(ctx, schema)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to create the database's tables: %w", err)
	}

	return &db{conn: conn}, nil
}

// Close the database
func (d *db) Close() error {
	err := d.conn.Close()
	if err != nil {
		return fmt.Errorf("failed to close the database: %w", err)
	}
	return nil
}

// Read a setting, reporting false when it isn't stored
func (d *db) setting(ctx context.Context, name string) (string, bool, error) {
	var value string
	err := d.conn.
		QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, name).
		Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	} else if err != nil {
		return "", false, fmt.Errorf("failed to read the setting '%s': %w", name, err)
	}

	return value, true, nil
}

// Store a setting
func (d *db) setSetting(ctx context.Context, name string, value string) error {
	_, err := d.conn.ExecContext(ctx, `INSERT INTO settings (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`, name, value)
	if err != nil {
		return fmt.Errorf("failed to store the setting '%s': %w", name, err)
	}

	return nil
}

// Apply an event to its image, unless it's a repeat of a delivery already received or older than what the image already shows
// Deliveries are at least once and may arrive out of order, so the webhook ID and the timestamp decide what an event changes
func (d *db) applyEvent(ctx context.Context, webhookID string, e *webhook.Event, nc notifyContext) (string, error) {
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to start a transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// A delivery with the same webhook ID that the app acknowledged before is a repeat, while one it failed is retried and applied now
	var repeats int
	err = tx.
		QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE webhook_id = ? AND status >= 200 AND status < 300`, webhookID).
		Scan(&repeats)
	if err != nil {
		return "", fmt.Errorf("failed to look up the delivery: %w", err)
	}
	if webhookID != "" && repeats > 0 {
		return outcomeRepeat, nil
	}

	// An event older than the one the image shows is stale
	var updatedAt int64
	err = tx.
		QueryRowContext(ctx, `SELECT updated_at FROM images WHERE namespace = ? AND image_id = ?`, e.Data.Namespace, e.Data.ImageID).
		Scan(&updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Nop
	case err != nil:
		return "", fmt.Errorf("failed to read the image: %w", err)
	case e.Timestamp.UnixNano() < updatedAt:
		return outcomeStale, nil
	}

	// Each account's images are in the namespace named after it, which is also where an event without a context belongs
	owner := nc.Owner
	if owner == "" {
		owner = e.Data.Namespace
	}
	var manifestJSON []byte
	if e.Data.Manifest != nil {
		manifestJSON, err = json.Marshal(e.Data.Manifest)
		if err != nil {
			return "", fmt.Errorf("failed to encode the manifest: %w", err)
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO images (namespace, image_id, owner, profile, status, job_id, error, manifest, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (namespace, image_id) DO UPDATE SET
			owner = excluded.owner, profile = excluded.profile, status = excluded.status, job_id = excluded.job_id,
			error = excluded.error, manifest = excluded.manifest, updated_at = excluded.updated_at`,
		e.Data.Namespace, e.Data.ImageID, owner, nc.Profile, string(e.Type), e.Data.JobID, e.Data.Error, nullableText(manifestJSON), e.Timestamp.UnixNano(),
	)
	if err != nil {
		return "", fmt.Errorf("failed to store the image: %w", err)
	}

	err = tx.Commit()
	if err != nil {
		return "", fmt.Errorf("failed to store the image: %w", err)
	}

	return outcomeApplied, nil
}

// List an account's images, newest first
func (d *db) images(ctx context.Context, owner string) ([]imageRecord, error) {
	rows, err := d.conn.QueryContext(ctx, `
		SELECT namespace, image_id, owner, profile, status, job_id, error, manifest, updated_at
		FROM images WHERE owner = ? ORDER BY updated_at DESC`, owner)
	if err != nil {
		return nil, fmt.Errorf("failed to list the images: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	out := make([]imageRecord, 0)
	for rows.Next() {
		var (
			img          imageRecord
			status       string
			manifestJSON sql.NullString
			updatedAt    int64
		)
		err = rows.Scan(&img.Namespace, &img.ImageID, &img.Owner, &img.Profile, &status, &img.JobID, &img.Error, &manifestJSON, &updatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to read an image: %w", err)
		}
		img.Status = webhook.EventType(status)
		img.UpdatedAt = time.Unix(0, updatedAt).UTC()
		if manifestJSON.Valid {
			var m manifest.Manifest
			err = json.Unmarshal([]byte(manifestJSON.String), &m)
			if err != nil {
				return nil, fmt.Errorf("failed to decode the manifest of image '%s': %w", img.ImageID, err)
			}
			img.Manifest = &m
		}

		out = append(out, img)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("failed to list the images: %w", err)
	}

	return out, nil
}

// Record a request provino made to the authorizer
func (d *db) recordAuthorization(ctx context.Context, rec authorizationRecord) error {
	headers, err := json.Marshal(rec.RequestHeaders)
	if err != nil {
		return fmt.Errorf("failed to encode the headers: %w", err)
	}

	_, err = d.conn.ExecContext(ctx, `
		INSERT INTO authorizations (received_at, subject, request_headers, request_body, status, response_body)
		VALUES (?, ?, ?, ?, ?, ?)`,
		rec.ReceivedAt.UnixNano(), rec.Subject, string(headers), string(rec.Request), rec.Status, string(rec.Response),
	)
	if err != nil {
		return fmt.Errorf("failed to record the authorization request: %w", err)
	}

	return nil
}

// List the latest authorizer requests made for an account's uploads, newest first
func (d *db) authorizations(ctx context.Context, subject string) ([]authorizationRecord, error) {
	rows, err := d.conn.QueryContext(ctx, `
		SELECT id, received_at, subject, request_headers, request_body, status, response_body
		FROM authorizations WHERE subject = ? ORDER BY id DESC LIMIT ?`, subject, listLimit)
	if err != nil {
		return nil, fmt.Errorf("failed to list the authorization requests: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	out := make([]authorizationRecord, 0)
	for rows.Next() {
		var (
			rec        authorizationRecord
			receivedAt int64
			headers    string
			request    string
			response   string
		)
		err = rows.Scan(&rec.ID, &receivedAt, &rec.Subject, &headers, &request, &rec.Status, &response)
		if err != nil {
			return nil, fmt.Errorf("failed to read an authorization request: %w", err)
		}

		rec.ReceivedAt = time.Unix(0, receivedAt).UTC()
		rec.RequestHeaders = decodeHeaders(headers)
		rec.Request = asJSON(request)
		rec.Response = asJSON(response)

		out = append(out, rec)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("failed to list the authorization requests: %w", err)
	}

	return out, nil
}

// Record a webhook delivery
func (d *db) recordDelivery(ctx context.Context, rec deliveryRecord) error {
	headers, err := json.Marshal(rec.RequestHeaders)
	if err != nil {
		return fmt.Errorf("failed to encode the headers: %w", err)
	}

	_, err = d.conn.ExecContext(ctx, `
		INSERT INTO deliveries (received_at, webhook_id, event_type, namespace, image_id, job_id, event_timestamp, request_headers, request_body, status, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ReceivedAt.UnixNano(), rec.WebhookID, string(rec.Type), rec.Namespace, rec.ImageID, rec.JobID, rec.Timestamp.UnixNano(), string(headers), string(rec.Body), rec.Status, rec.Outcome,
	)
	if err != nil {
		return fmt.Errorf("failed to record the delivery: %w", err)
	}

	return nil
}

// List the latest deliveries about the images in an account's namespace, newest first
func (d *db) deliveries(ctx context.Context, namespace string) ([]deliveryRecord, error) {
	rows, err := d.conn.QueryContext(ctx, `
		SELECT id, received_at, webhook_id, event_type, namespace, image_id, job_id, event_timestamp, request_headers, request_body, status, outcome
		FROM deliveries WHERE namespace = ? ORDER BY id DESC LIMIT ?`, namespace, listLimit)
	if err != nil {
		return nil, fmt.Errorf("failed to list the deliveries: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	out := make([]deliveryRecord, 0)
	for rows.Next() {
		var (
			rec        deliveryRecord
			receivedAt int64
			eventType  string
			timestamp  int64
			headers    string
			body       string
		)
		err = rows.Scan(&rec.ID, &receivedAt, &rec.WebhookID, &eventType, &rec.Namespace, &rec.ImageID, &rec.JobID, &timestamp, &headers, &body, &rec.Status, &rec.Outcome)
		if err != nil {
			return nil, fmt.Errorf("failed to read a delivery: %w", err)
		}
		rec.ReceivedAt = time.Unix(0, receivedAt).UTC()
		rec.Type = webhook.EventType(eventType)
		rec.Timestamp = time.Unix(0, timestamp).UTC()
		rec.RequestHeaders = decodeHeaders(headers)
		rec.Body = asJSON(body)
		out = append(out, rec)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("failed to list the deliveries: %w", err)
	}

	return out, nil
}

// Store an empty value as NULL
func nullableText(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// Decode stored headers, which the app wrote itself
func decodeHeaders(s string) map[string]string {
	headers := make(map[string]string)
	_ = json.Unmarshal([]byte(s), &headers)
	return headers
}

// Return a stored body as JSON: as it is when it's valid JSON, and as a JSON string otherwise, such as for a plain-text error
func asJSON(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}

	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}

	quoted, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage("null")
	}

	return quoted
}
