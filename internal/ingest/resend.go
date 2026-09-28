package ingest

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"

	svix "github.com/svix/svix-webhooks/go"

	"github.com/chromano/typedmail/internal/store"
)

// resendEvent is the subset of Resend's webhook payload we use. For
// email.received it carries metadata only; the body is fetched later.
// https://resend.com/docs/webhooks/emails/received
type resendEvent struct {
	Type string
	Data struct {
		EmailID   string `json:"email_id"`
		From      string
		To        []string
		Subject   string
		MessageID string `json:"message_id"`
	}
}

type ResendHandler struct {
	saver   Saver
	webhook *svix.Webhook
	log     *slog.Logger
}

// NewResendHandler returns the Resend webhook handler. secret is the
// webhook's signing secret (whsec_...), shown in Resend when the webhook is
// created.
func NewResendHandler(saver Saver, secret string, log *slog.Logger) (*ResendHandler, error) {
	wh, err := svix.NewWebhook(secret)
	if err != nil {
		return nil, err
	}
	return &ResendHandler{saver: saver, webhook: wh, log: log}, nil
}

func (h *ResendHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	// The signature covers the exact bytes received, so verify before
	// touching them. It also rejects requests older than 5 minutes, which
	// stops replays.
	if err := h.webhook.Verify(raw, r.Header); err != nil {
		h.log.Warn("rejected Resend webhook with an invalid signature", "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var ev resendEvent
	raw, err = stripNUL(raw)
	if err == nil {
		err = json.Unmarshal(raw, &ev)
	}
	if err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if ev.Type != "email.received" {
		// Other events (sent, delivered, ...) may share the endpoint.
		writeJSON(w, map[string]any{"status": "ignored"})
		return
	}

	msg, err := resendToInbound(ev, raw)
	if err != nil {
		h.log.Warn("rejected inbound message", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	save(w, r, h.saver, h.log, msg)
}

func resendToInbound(ev resendEvent, raw []byte) (store.InboundMessage, error) {
	d := ev.Data
	if d.EmailID == "" {
		return store.InboundMessage{}, errors.New("missing email_id")
	}
	from := address(d.From)
	if from == "" {
		return store.InboundMessage{}, errors.New("missing from")
	}
	if len(d.To) == 0 {
		return store.InboundMessage{}, errors.New("missing to")
	}
	// orders@<id>.resend.app, or orders@mail.example.com on a custom domain,
	// goes to the orders inbox. With a +tag, the tag picks the inbox, as with
	// Postmark: support+invoices@mail.example.com goes to invoices.
	local, _, found := strings.Cut(address(d.To[0]), "@")
	if _, tag, ok := strings.Cut(local, "+"); ok && tag != "" {
		local = tag
	}
	if !found || local == "" {
		return store.InboundMessage{}, errors.New("cannot determine inbox from recipient")
	}
	// The sender's Message-ID matches the one Postmark would see, so the same
	// email arriving through both providers is stored once.
	id := strings.TrimSpace(d.MessageID)
	if id == "" {
		id = "resend:" + d.EmailID
	}
	return store.InboundMessage{
		Provider:    store.ProviderResend,
		ProviderID:  d.EmailID,
		InboxSlug:   strings.ToLower(local),
		MessageID:   id,
		FromAddress: from,
		Subject:     d.Subject,
		Raw:         raw,
	}, nil
}

// address returns the bare address from "Name <user@host>" or "user@host".
func address(s string) string {
	if a, err := mail.ParseAddress(s); err == nil {
		return a.Address
	}
	return strings.TrimSpace(s)
}
