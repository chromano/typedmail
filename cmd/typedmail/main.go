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
	"github.com/chromano/typedmail/internal/resend"
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
	if cfg.resendWebhookSecret != "" {
		rh, err := ingest.NewResendHandler(st, cfg.resendWebhookSecret, log)
		if err != nil {
			return fmt.Errorf("RESEND_WEBHOOK_SECRET: %w", err)
		}
		mux.Handle("POST /webhooks/resend", rh)
	}
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

	// Each job kind runs only when its API key is set; jobs of other kinds
	// stay queued until one is configured.
	handlers := map[string]worker.Handler{}
	if cfg.anthropicAPIKey != "" {
		handlers[store.JobExtract] = extractHandler(st, extract.New(cfg.extractModel), log)
	} else {
		log.Warn("ANTHROPIC_API_KEY not set; extraction is disabled and extract jobs stay queued")
	}
	if cfg.resendAPIKey != "" {
		rc := resend.New(cfg.resendAPIKey)
		if u := os.Getenv("RESEND_BASE_URL"); u != "" {
			rc.WithBaseURL(u) // e.g. a local fake, like ANTHROPIC_BASE_URL
		}
		handlers[store.JobFetch] = fetchHandler(st, rc)
	} else if cfg.resendWebhookSecret != "" {
		log.Warn("RESEND_API_KEY not set; Resend messages are stored but their bodies aren't fetched")
	}

	workerDone := make(chan struct{})
	if len(handlers) == 0 {
		close(workerDone)
	} else {
		wrk := worker.New(st, handlers, worker.Config{}, log)
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

	resendWebhookSecret string
	resendAPIKey        string
}

func loadConfig() (config, error) {
	cfg := config{
		port:            getenv("PORT", "8080"),
		databaseURL:     os.Getenv("DATABASE_URL"),
		inboundUser:     os.Getenv("INBOUND_USER"),
		inboundPassword: os.Getenv("INBOUND_PASSWORD"),
		anthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		extractModel:    getenv("EXTRACT_MODEL", extract.DefaultModel),

		resendWebhookSecret: os.Getenv("RESEND_WEBHOOK_SECRET"),
		resendAPIKey:        os.Getenv("RESEND_API_KEY"),
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
		if err := st.SaveExtraction(ctx, job.MessageID, out.Model, out.JSON, out.ValidationErrors); err != nil {
			return fmt.Errorf("save extraction: %w", err)
		}
		if len(out.ValidationErrors) > 0 {
			log.Warn("extracted, but the result breaks the inbox schema", "message_id", job.MessageID,
				"inbox", in.InboxSlug, "model", out.Model, "validation_errors", out.ValidationErrors)
			return nil
		}
		log.Info("extracted", "message_id", job.MessageID, "inbox", in.InboxSlug, "model", out.Model)
		return nil
	}
}

// fetchHandler gets a Resend message's body, which the webhook omits, then
// stores it and queues extraction.
func fetchHandler(st *store.Store, rc *resend.Client) worker.Handler {
	return func(ctx context.Context, job store.Job) error {
		emailID, err := st.ProviderID(ctx, job.MessageID)
		if err != nil {
			return fmt.Errorf("load message: %w", err)
		}
		body, err := rc.ReceivedEmail(ctx, emailID)
		var se *resend.StatusError
		if errors.As(err, &se) && se.Permanent() {
			return worker.Permanent(err)
		}
		if err != nil {
			return err
		}
		if err := st.SaveBody(ctx, job.MessageID, body.Text, body.HTML); err != nil {
			return fmt.Errorf("save body: %w", err)
		}
		return nil
	}
}
