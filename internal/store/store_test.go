package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

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

// saveJobs stores n distinct messages, each with its queued extract job.
func saveJobs(t *testing.T, s *Store, n int) {
	t.Helper()
	for i := range n {
		m := msg
		m.MessageID = fmt.Sprintf("<job-%d@example.com>", i)
		if _, _, err := s.SaveInbound(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
}

type jobRow struct {
	status    string
	attempts  int
	lastError *string
	due       bool
}

func getJob(t *testing.T, pool *pgxpool.Pool, id int64) jobRow {
	t.Helper()
	var r jobRow
	err := pool.QueryRow(context.Background(),
		"SELECT status, attempts, last_error, run_at <= now() FROM jobs WHERE id = $1", id,
	).Scan(&r.status, &r.attempts, &r.lastError, &r.due)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestClaimJob(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	saveJobs(t, s, 1)

	job, ok, err := s.ClaimJob(ctx, time.Hour)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if job.Kind != JobExtract || job.Attempts != 1 || job.MessageID == 0 {
		t.Errorf("job = %+v", job)
	}
	if r := getJob(t, pool, job.ID); r.status != "running" || r.attempts != 1 {
		t.Errorf("row = %+v, want running with 1 attempt", r)
	}

	if _, ok, err := s.ClaimJob(ctx, time.Hour); err != nil || ok {
		t.Errorf("second claim: ok=%v err=%v, want no job", ok, err)
	}
}

func TestClaimJobSkipsJobsNotYetDue(t *testing.T) {
	s, pool := newTestStore(t)
	saveJobs(t, s, 1)
	if _, err := pool.Exec(context.Background(), "UPDATE jobs SET run_at = now() + interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimJob(context.Background(), time.Hour); err != nil || ok {
		t.Errorf("claim: ok=%v err=%v, want no job", ok, err)
	}
}

func TestClaimJobInParallelTakesEachJobOnce(t *testing.T) {
	s, _ := newTestStore(t)
	const n = 20
	saveJobs(t, s, n)

	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, ok, err := s.ClaimJob(context.Background(), time.Hour)
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				seen[job.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != n {
		t.Errorf("claimed %d distinct jobs, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("job %d claimed %d times", id, c)
		}
	}
}

func TestJobOutcomes(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	saveJobs(t, s, 3)

	var ids []int64
	for range 3 {
		job, ok, err := s.ClaimJob(ctx, time.Hour)
		if err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		ids = append(ids, job.ID)
	}

	if err := s.CompleteJob(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryJob(ctx, ids[1], "timeout", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.FailJob(ctx, ids[2], "bad input"); err != nil {
		t.Fatal(err)
	}

	if r := getJob(t, pool, ids[0]); r.status != "done" || r.lastError != nil {
		t.Errorf("completed job = %+v", r)
	}
	if r := getJob(t, pool, ids[1]); r.status != "queued" || r.due || r.lastError == nil || *r.lastError != "timeout" {
		t.Errorf("retried job = %+v, want queued, not yet due, last_error timeout", r)
	}
	if r := getJob(t, pool, ids[2]); r.status != "failed" || r.lastError == nil || *r.lastError != "bad input" {
		t.Errorf("failed job = %+v", r)
	}

	// The retried job isn't due for an hour, so nothing can be claimed.
	if _, ok, err := s.ClaimJob(ctx, time.Hour); err != nil || ok {
		t.Errorf("claim: ok=%v err=%v, want no job", ok, err)
	}
}

func TestClaimJobReclaimsStaleRunningJobs(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	saveJobs(t, s, 1)

	first, ok, err := s.ClaimJob(ctx, time.Hour)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	// Running for less than staleAfter: the worker may still be on it.
	if _, ok, err := s.ClaimJob(ctx, time.Hour); err != nil || ok {
		t.Fatalf("claim of a live job: ok=%v err=%v, want no job", ok, err)
	}

	// Pretend the worker died two hours ago.
	if _, err := pool.Exec(ctx, "UPDATE jobs SET updated_at = now() - interval '2 hours'"); err != nil {
		t.Fatal(err)
	}
	second, ok, err := s.ClaimJob(ctx, time.Hour)
	if err != nil || !ok {
		t.Fatalf("claim of a stale job: ok=%v err=%v", ok, err)
	}
	if second.ID != first.ID || second.Attempts != 2 {
		t.Errorf("reclaimed %+v, want job %d on attempt 2", second, first.ID)
	}
}
