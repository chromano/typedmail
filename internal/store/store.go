// Package store is the Postgres persistence layer.
package store

import (
	"context"
	"errors"

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

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}
