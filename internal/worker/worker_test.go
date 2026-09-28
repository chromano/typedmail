package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/chromano/typedmail/internal/store"
)

// fakeQueue hands out its jobs once each and records what happened to them.
type fakeQueue struct {
	mu        sync.Mutex
	jobs      []store.Job
	completed []int64
	failed    map[int64]string
	retried   map[int64]time.Duration
}

func newFakeQueue(jobs ...store.Job) *fakeQueue {
	return &fakeQueue{jobs: jobs, failed: map[int64]string{}, retried: map[int64]time.Duration{}}
}

func (q *fakeQueue) ClaimJob(context.Context) (store.Job, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.jobs) == 0 {
		return store.Job{}, false, nil
	}
	j := q.jobs[0]
	q.jobs = q.jobs[1:]
	return j, true, nil
}

func (q *fakeQueue) CompleteJob(_ context.Context, id int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.completed = append(q.completed, id)
	return nil
}

func (q *fakeQueue) RetryJob(_ context.Context, id int64, _ string, delay time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.retried[id] = delay
	return nil
}

func (q *fakeQueue) FailJob(_ context.Context, id int64, lastError string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failed[id] = lastError
	return nil
}

func (q *fakeQueue) outcomes() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.completed) + len(q.failed) + len(q.retried)
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// runUntil runs the worker until every job has an outcome, then stops it.
func runUntil(t *testing.T, w *Worker, q *fakeQueue, n int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	deadline := time.After(5 * time.Second)
	for q.outcomes() < n {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("only %d of %d jobs finished", q.outcomes(), n)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestWorkerRecordsOutcomes(t *testing.T) {
	q := newFakeQueue(
		store.Job{ID: 1, Kind: "ok", Attempts: 1},
		store.Job{ID: 2, Kind: "flaky", Attempts: 1},
		store.Job{ID: 3, Kind: "flaky", Attempts: 3},
		store.Job{ID: 4, Kind: "flaky", Attempts: 5},
		store.Job{ID: 5, Kind: "broken", Attempts: 1},
		store.Job{ID: 6, Kind: "panics", Attempts: 1},
		store.Job{ID: 7, Kind: "unknown", Attempts: 1},
	)
	w := New(q, map[string]Handler{
		"ok":     func(context.Context, store.Job) error { return nil },
		"flaky":  func(context.Context, store.Job) error { return errors.New("timeout") },
		"broken": func(context.Context, store.Job) error { return Permanent(errors.New("bad input")) },
		"panics": func(context.Context, store.Job) error { panic("boom") },
	}, Config{PollInterval: time.Millisecond, MaxAttempts: 5, BaseBackoff: time.Minute, MaxBackoff: time.Hour}, discard)

	runUntil(t, w, q, 7)

	if len(q.completed) != 1 || q.completed[0] != 1 {
		t.Errorf("completed = %v, want [1]", q.completed)
	}
	wantRetried := map[int64]time.Duration{2: time.Minute, 3: 4 * time.Minute, 6: time.Minute}
	for id, want := range wantRetried {
		if got, ok := q.retried[id]; !ok || got != want {
			t.Errorf("job %d retried in %v (retried=%v), want %v", id, got, ok, want)
		}
	}
	wantFailed := map[int64]string{4: "timeout", 5: "bad input", 7: `no handler for job kind "unknown"`}
	for id, want := range wantFailed {
		if got := q.failed[id]; got != want {
			t.Errorf("job %d failed with %q, want %q", id, got, want)
		}
	}
	if len(q.retried) != len(wantRetried) || len(q.failed) != len(wantFailed) {
		t.Errorf("retried = %v, failed = %v", q.retried, q.failed)
	}
}

func TestBackoffIsCapped(t *testing.T) {
	w := New(nil, nil, Config{BaseBackoff: 30 * time.Second, MaxBackoff: time.Hour}, discard)
	tests := map[int]time.Duration{
		1:  30 * time.Second,
		2:  time.Minute,
		5:  8 * time.Minute,
		8:  time.Hour, // 64m, capped
		60: time.Hour,
	}
	for attempt, want := range tests {
		if got := w.backoff(attempt); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}

func TestShutdownWaitsForRunningJob(t *testing.T) {
	q := newFakeQueue(store.Job{ID: 1, Kind: "slow", Attempts: 1})
	started := make(chan struct{})
	w := New(q, map[string]Handler{
		"slow": func(ctx context.Context, _ store.Job) error {
			close(started)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
				return nil
			}
		},
	}, Config{PollInterval: time.Millisecond}, discard)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	<-started
	cancel()
	<-done

	if len(q.completed) != 1 {
		t.Errorf("completed = %v, want the running job to finish despite shutdown", q.completed)
	}
}

func TestJobTimeout(t *testing.T) {
	q := newFakeQueue(store.Job{ID: 1, Kind: "hangs", Attempts: 1})
	w := New(q, map[string]Handler{
		"hangs": func(ctx context.Context, _ store.Job) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}, Config{PollInterval: time.Millisecond, JobTimeout: 10 * time.Millisecond}, discard)

	runUntil(t, w, q, 1)

	if _, ok := q.retried[1]; !ok {
		t.Errorf("retried = %v, want the timed-out job retried", q.retried)
	}
}
