package store

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chromano/typedmail/migrations"
)

// newTestStore connects to TEST_DATABASE_URL, migrates it and empties it.
// The database is wiped, so never point this at real data.
func newTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	if err := Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE jobs, messages, inboxes RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO inboxes (slug) VALUES ('orders')"); err != nil {
		t.Fatal(err)
	}
	return New(pool), pool
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var msg = InboundMessage{
	InboxSlug:   "orders",
	MessageID:   "<po10442@mail.acme-retail.com>",
	FromAddress: "buyer@acme-retail.com",
	Subject:     "PO 10442",
	TextBody:    "Please ship 200 stickers.",
	Raw:         []byte(`{"From":"buyer@acme-retail.com"}`),
}

func TestSaveInboundStoresMessageAndEnqueuesJob(t *testing.T) {
	s, pool := newTestStore(t)

	id, created, err := s.SaveInbound(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if !created || id == 0 {
		t.Fatalf("got id=%d created=%v, want a new message", id, created)
	}

	var kind, status string
	err = pool.QueryRow(context.Background(),
		"SELECT kind, status FROM jobs WHERE message_id = $1", id).Scan(&kind, &status)
	if err != nil {
		t.Fatal(err)
	}
	if kind != JobExtract || status != "queued" {
		t.Errorf("job = (%s, %s), want (%s, queued)", kind, status, JobExtract)
	}
}

func TestSaveInboundIgnoresDuplicates(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.SaveInbound(ctx, msg); err != nil {
		t.Fatal(err)
	}
	_, created, err := s.SaveInbound(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("second save reported created = true")
	}
	if n := count(t, pool, "messages"); n != 1 {
		t.Errorf("messages = %d, want 1", n)
	}
	if n := count(t, pool, "jobs"); n != 1 {
		t.Errorf("jobs = %d, want 1", n)
	}
}

func TestSaveInboundUnknownInbox(t *testing.T) {
	s, pool := newTestStore(t)

	m := msg
	m.InboxSlug = "nope"
	if _, _, err := s.SaveInbound(context.Background(), m); !errors.Is(err, ErrUnknownInbox) {
		t.Fatalf("err = %v, want ErrUnknownInbox", err)
	}
	if n := count(t, pool, "messages"); n != 0 {
		t.Errorf("messages = %d, want 0", n)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	_, pool := newTestStore(t) // already migrated once
	if err := Migrate(context.Background(), pool, migrations.FS); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}
