package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chromano/typedmail/internal/extract"
	"github.com/chromano/typedmail/internal/ingest"
	"github.com/chromano/typedmail/internal/store"
	"github.com/chromano/typedmail/internal/worker"
	"github.com/chromano/typedmail/migrations"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	st := store.New(pool)

	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/postmark", ingest.NewHandler(st, cfg.inboundUser, cfg.inboundPassword, log))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.Ping(r.Context()); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	workerDone := make(chan struct{})
	if cfg.anthropicAPIKey == "" {
		// Without a key every extraction would fail; leave jobs queued until
		// one is configured.
		log.Warn("ANTHROPIC_API_KEY not set; extraction is disabled and jobs stay queued")
		close(workerDone)
	} else {
		ex := extract.New(cfg.extractModel)
		wrk := worker.New(st, map[string]worker.Handler{
			store.JobExtract: extractHandler(st, ex, log),
		}, worker.Config{}, log)
		go func() {
			defer close(workerDone)
			wrk.Run(ctx)
		}()
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		errc <- srv.ListenAndServe()
	}()

	var serveErr error
	select {
	case serveErr = <-errc:
		stop() // stop the worker too
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-workerDone // let running jobs finish and record their outcome
	return serveErr
}

type config struct {
	port            string
	databaseURL     string
	inboundUser     string
	inboundPassword string
	anthropicAPIKey string
	extractModel    string
}

func loadConfig() (config, error) {
	cfg := config{
		port:            getenv("PORT", "8080"),
		databaseURL:     os.Getenv("DATABASE_URL"),
		inboundUser:     os.Getenv("INBOUND_USER"),
		inboundPassword: os.Getenv("INBOUND_PASSWORD"),
		anthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		extractModel:    getenv("EXTRACT_MODEL", extract.DefaultModel),
	}
	if cfg.databaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if cfg.inboundUser == "" || cfg.inboundPassword == "" {
		return cfg, errors.New("INBOUND_USER and INBOUND_PASSWORD are required")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// extractHandler runs extraction for a message and stores the result.
func extractHandler(st *store.Store, ex *extract.Extractor, log *slog.Logger) worker.Handler {
	return func(ctx context.Context, job store.Job) error {
		in, err := st.ExtractInput(ctx, job.MessageID)
		if err != nil {
			return fmt.Errorf("load message: %w", err)
		}
		out, err := ex.Extract(ctx, in.Schema, extract.Email{
			From:     in.FromAddress,
			Subject:  in.Subject,
			TextBody: in.TextBody,
			HTMLBody: in.HTMLBody,
		})
		if errors.Is(err, extract.ErrUnextractable) {
			return worker.Permanent(err)
		}
		if err != nil {
			return err
		}
		if err := st.SaveExtraction(ctx, job.MessageID, out.Model, out.JSON); err != nil {
			return fmt.Errorf("save extraction: %w", err)
		}
		log.Info("extracted", "message_id", job.MessageID, "inbox", in.InboxSlug, "model", out.Model)
		return nil
	}
}
