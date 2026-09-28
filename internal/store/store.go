// Package store is the Postgres persistence layer.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUnknownInbox is returned when a message is addressed to an inbox slug
// that doesn't exist.
var ErrUnknownInbox = errors.New("unknown inbox")

// Job kinds. A Postmark message starts with extract; a Resend message starts
// with fetch, which gets the body (Resend's webhook omits it) and then
// queues extract.
const (
	JobExtract = "extract"
	JobFetch   = "fetch"
)

// Providers that deliver inbound email.
const (
	ProviderPostmark = "postmark"
	ProviderResend   = "resend"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// InboundMessage is a received email, already mapped from the provider's
// payload into our own shape.
type InboundMessage struct {
	Provider    string // ProviderPostmark or ProviderResend
	ProviderID  string // the provider's own ID for the message
	InboxSlug   string
	MessageID   string
	FromAddress string
	Subject     string
	TextBody    string
	HTMLBody    string
	Raw         []byte // the provider's payload, verbatim
}

// SaveInbound stores the message and enqueues its first job (fetch for
// Resend, extract otherwise) in one transaction. created is false when the message was already stored (a
// duplicate delivery), in which case nothing is written.
func (s *Store) SaveInbound(ctx context.Context, m InboundMessage) (id int64, created bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var inboxID int64
		err := tx.QueryRow(ctx, "SELECT id FROM inboxes WHERE slug = $1", m.InboxSlug).Scan(&inboxID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownInbox
		}
		if err != nil {
			return err
		}

		err = tx.QueryRow(ctx, `
			INSERT INTO messages (inbox_id, message_id, provider, provider_id, from_address, subject, text_body, html_body, raw)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (inbox_id, message_id) DO NOTHING
			RETURNING id`,
			inboxID, m.MessageID, m.Provider, m.ProviderID, m.FromAddress, m.Subject, m.TextBody, m.HTMLBody, m.Raw,
		).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // duplicate
		}
		if err != nil {
			return err
		}
		created = true

		kind := JobExtract
		if m.Provider == ProviderResend {
			kind = JobFetch
		}
		_, err = tx.Exec(ctx, "INSERT INTO jobs (kind, message_id) VALUES ($1, $2)", kind, id)
		return err
	})
	return id, created, err
}

// ProviderID returns the provider's own ID for a message.
func (s *Store) ProviderID(ctx context.Context, messageID int64) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, "SELECT provider_id FROM messages WHERE id = $1", messageID).Scan(&id)
	return id, err
}

// SaveBody stores a fetched message body and enqueues its extraction, in one
// transaction.
func (s *Store) SaveBody(ctx context.Context, messageID int64, text, html string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			"UPDATE messages SET text_body = $2, html_body = $3 WHERE id = $1", messageID, text, html); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO jobs (kind, message_id) VALUES ($1, $2)", JobExtract, messageID)
		return err
	})
}

// ExtractInput is a stored message together with its inbox's schema.
type ExtractInput struct {
	InboxSlug   string
	Schema      json.RawMessage
	FromAddress string
	Subject     string
	TextBody    string
	HTMLBody    string
}

// ExtractInput loads what extraction needs for one message.
func (s *Store) ExtractInput(ctx context.Context, messageID int64) (in ExtractInput, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT i.slug, i.schema, m.from_address, m.subject, m.text_body, m.html_body
		FROM messages m JOIN inboxes i ON i.id = m.inbox_id
		WHERE m.id = $1`, messageID,
	).Scan(&in.InboxSlug, &in.Schema, &in.FromAddress, &in.Subject, &in.TextBody, &in.HTMLBody)
	return in, err
}

// SaveExtraction stores the JSON extracted from a message, replacing any
// earlier result.
func (s *Store) SaveExtraction(ctx context.Context, messageID int64, model string, data json.RawMessage) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO extractions (message_id, model, data) VALUES ($1, $2, $3)
		ON CONFLICT (message_id) DO UPDATE
		SET model = EXCLUDED.model, data = EXCLUDED.data, created_at = now()`,
		messageID, model, data)
	return err
}

// Job is a claimed unit of work. Attempts counts this run.
type Job struct {
	ID        int64
	Kind      string
	MessageID int64
	Attempts  int
}

// ClaimJob marks the oldest due job of one of kinds as running and returns
// it. ok is false when no such job is due; jobs of other kinds are left for a
// worker that can run them. A job is due when it is queued and its run_at has
// passed, or when it has been running for longer than staleAfter, which means
// the worker running it died. staleAfter must be longer than any run can
// take, or a slow job would be run twice.
//
// SKIP LOCKED lets several workers claim in parallel without taking the same
// job, and the claim commits right away so the job itself runs outside any
// transaction. updated_at records when the job was claimed; nothing else
// writes to a running job.
func (s *Store) ClaimJob(ctx context.Context, kinds []string, staleAfter time.Duration) (job Job, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `
		UPDATE jobs SET status = 'running', attempts = attempts + 1, updated_at = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE kind = ANY($2)
			  AND ((status = 'queued' AND run_at <= now())
			    OR (status = 'running' AND updated_at < now() - $1 * interval '1 millisecond'))
			ORDER BY run_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, kind, message_id, attempts`,
		staleAfter.Milliseconds(), kinds,
	).Scan(&job.ID, &job.Kind, &job.MessageID, &job.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	return job, err == nil, err
}

// CompleteJob marks a running job as done.
func (s *Store) CompleteJob(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE jobs SET status = 'done', last_error = NULL, updated_at = now() WHERE id = $1", id)
	return err
}

// RetryJob puts a failed job back in the queue, due after delay.
func (s *Store) RetryJob(ctx context.Context, id int64, lastError string, delay time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status = 'queued', last_error = $2,
			run_at = now() + $3 * interval '1 millisecond', updated_at = now()
		WHERE id = $1`,
		id, lastError, delay.Milliseconds())
	return err
}

// FailJob marks a job as failed for good.
func (s *Store) FailJob(ctx context.Context, id int64, lastError string) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE jobs SET status = 'failed', last_error = $2, updated_at = now() WHERE id = $1",
		id, lastError)
	return err
}

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}
