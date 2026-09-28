// Package store is the Postgres persistence layer.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUnknownInbox is returned when a message is addressed to an inbox slug
// that doesn't exist.
var ErrUnknownInbox = errors.New("unknown inbox")

// JobExtract is the job kind that runs extraction on a newly received message.
const JobExtract = "extract"

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// InboundMessage is a received email, already mapped from the provider's
// payload into our own shape.
type InboundMessage struct {
	InboxSlug   string
	MessageID   string
	FromAddress string
	Subject     string
	TextBody    string
	HTMLBody    string
	Raw         []byte // the provider's payload, verbatim
}

// SaveInbound stores the message and enqueues its extraction job in one
// transaction. created is false when the message was already stored (a
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
			INSERT INTO messages (inbox_id, message_id, from_address, subject, text_body, html_body, raw)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (inbox_id, message_id) DO NOTHING
			RETURNING id`,
			inboxID, m.MessageID, m.FromAddress, m.Subject, m.TextBody, m.HTMLBody, m.Raw,
		).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // duplicate
		}
		if err != nil {
			return err
		}
		created = true

		_, err = tx.Exec(ctx, "INSERT INTO jobs (kind, message_id) VALUES ($1, $2)", JobExtract, id)
		return err
	})
	return id, created, err
}

// Job is a claimed unit of work. Attempts counts this run.
type Job struct {
	ID        int64
	Kind      string
	MessageID int64
	Attempts  int
}

// ClaimJob marks the oldest due job as running and returns it. ok is false
// when no job is due. A job is due when it is queued and its run_at has
// passed, or when it has been running for longer than staleAfter, which means
// the worker running it died. staleAfter must be longer than any run can
// take, or a slow job would be run twice.
//
// SKIP LOCKED lets several workers claim in parallel without taking the same
// job, and the claim commits right away so the job itself runs outside any
// transaction. updated_at records when the job was claimed; nothing else
// writes to a running job.
func (s *Store) ClaimJob(ctx context.Context, staleAfter time.Duration) (job Job, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `
		UPDATE jobs SET status = 'running', attempts = attempts + 1, updated_at = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE (status = 'queued' AND run_at <= now())
			   OR (status = 'running' AND updated_at < now() - $1 * interval '1 millisecond')
			ORDER BY run_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, kind, message_id, attempts`,
		staleAfter.Milliseconds(),
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
