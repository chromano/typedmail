// Package worker drains the jobs queue: it claims due jobs, runs the handler
// for their kind, and records the outcome, retrying failures with backoff.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/chromano/typedmail/internal/store"
)

// Queue is the part of the store the worker needs.
type Queue interface {
	ClaimJob(ctx context.Context, staleAfter time.Duration) (store.Job, bool, error)
	CompleteJob(ctx context.Context, id int64) error
	RetryJob(ctx context.Context, id int64, lastError string, delay time.Duration) error
	FailJob(ctx context.Context, id int64, lastError string) error
}

// Handler runs one job. Returning an error retries the job, unless the error
// is wrapped with Permanent.
type Handler func(ctx context.Context, job store.Job) error

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks an error that retrying can't fix, so the job fails at once.
func Permanent(err error) error { return permanentError{err} }

// Config tunes the worker. The defaults spread 10 attempts over about 6 hours
// (1m, 2m, 4m ... 2h, 2h), so a job survives an outage of the LLM provider or
// the database.
type Config struct {
	Concurrency  int           // jobs run in parallel; default 4
	PollInterval time.Duration // wait when the queue is empty; default 1s
	MaxAttempts  int           // runs before a job is failed for good; default 10
	JobTimeout   time.Duration // limit for one run; default 5m. A job running for twice this long is reclaimed
	BaseBackoff  time.Duration // delay before the first retry, doubled after each; default 1m
	MaxBackoff   time.Duration // cap on the retry delay; default 2h
}

type Worker struct {
	queue    Queue
	handlers map[string]Handler
	cfg      Config
	log      *slog.Logger
}

func New(queue Queue, handlers map[string]Handler, cfg Config, log *slog.Logger) *Worker {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 10
	}
	if cfg.JobTimeout <= 0 {
		cfg.JobTimeout = 5 * time.Minute
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = time.Minute
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 2 * time.Hour
	}
	return &Worker{queue: queue, handlers: handlers, cfg: cfg, log: log}
}

// Run processes jobs until ctx is cancelled. It then stops claiming new jobs,
// cancels the running ones, and returns once their outcome is recorded. An
// interrupted job is retried like any other failure.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range w.cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		job, ok, err := w.queue.ClaimJob(ctx, w.staleAfter())
		if err != nil && ctx.Err() == nil {
			w.log.Error("claim job", "err", err)
		}
		if err != nil || !ok {
			select {
			case <-ctx.Done():
			case <-time.After(w.cfg.PollInterval):
			}
			continue
		}
		w.process(ctx, job)
	}
}

// process runs a claimed job. ctx is cancelled on shutdown; the outcome is
// recorded even then.
func (w *Worker) process(ctx context.Context, job store.Job) {
	log := w.log.With("job_id", job.ID, "kind", job.Kind, "message_id", job.MessageID, "attempt", job.Attempts)
	record := context.WithoutCancel(ctx)

	h, ok := w.handlers[job.Kind]
	var runErr, err error
	switch {
	case !ok:
		runErr = fmt.Errorf("no handler for job kind %q", job.Kind)
		log.Error("job failed", "err", runErr)
		err = w.queue.FailJob(record, job.ID, runErr.Error())
	case job.Attempts > w.cfg.MaxAttempts:
		// Reclaimed after its worker died on the last allowed attempt. Crashes
		// count, or a message that crashes the worker would loop forever.
		log.Error("job failed", "err", "worker died while running the job")
		err = w.queue.FailJob(record, job.ID, "worker died while running the job")
	default:
		start := time.Now()
		runErr = w.run(ctx, h, job)
		switch {
		case runErr == nil:
			log.Info("job done", "duration_ms", time.Since(start).Milliseconds())
			err = w.queue.CompleteJob(record, job.ID)
		case errors.As(runErr, new(permanentError)) || job.Attempts >= w.cfg.MaxAttempts:
			log.Error("job failed", "err", runErr)
			err = w.queue.FailJob(record, job.ID, runErr.Error())
		default:
			delay := w.backoff(job.Attempts)
			log.Warn("job will be retried", "err", runErr, "retry_in", delay)
			err = w.queue.RetryJob(record, job.ID, runErr.Error(), delay)
		}
	}
	if err != nil {
		log.Error("record job outcome", "err", err)
	}
}

func (w *Worker) run(ctx context.Context, h Handler, job store.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, w.cfg.JobTimeout)
	defer cancel()
	return h(ctx, job)
}

// staleAfter is how long a job may stay running before it is assumed dead and
// claimed again. It is derived from JobTimeout so the two can't drift apart:
// a live run is cancelled long before then.
func (w *Worker) staleAfter() time.Duration {
	return 2 * w.cfg.JobTimeout
}

// backoff doubles the delay after each attempt, up to MaxBackoff.
func (w *Worker) backoff(attempt int) time.Duration {
	d := w.cfg.BaseBackoff
	for i := 1; i < attempt && d < w.cfg.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, w.cfg.MaxBackoff)
}
