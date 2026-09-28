// Package ingest receives inbound email from the provider's webhook and hands
// it to the store. It does no processing beyond mapping the payload: the
// handler must answer fast, and extraction happens later in the worker.
package ingest

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/chromano/typedmail/internal/store"
)

// maxBodyBytes matches Postmark's 35 MB limit for inbound messages.
const maxBodyBytes = 35 << 20

// Saver is the part of the store the handler needs.
type Saver interface {
	SaveInbound(ctx context.Context, m store.InboundMessage) (id int64, created bool, err error)
}

// postmarkInbound is the subset of Postmark's inbound webhook payload we use.
// https://postmarkapp.com/developer/webhooks/inbound-webhook
type postmarkInbound struct {
	From              string
	Subject           string
	MessageID         string
	MailboxHash       string
	OriginalRecipient string
	TextBody          string
	HtmlBody          string
	Headers           []struct{ Name, Value string }
}

type Handler struct {
	saver    Saver
	user     string
	password string
	log      *slog.Logger
}

// NewHandler returns the Postmark webhook handler. Postmark sends the basic
// auth credentials embedded in the webhook URL.
func NewHandler(saver Saver, user, password string, log *slog.Logger) *Handler {
	return &Handler{saver: saver, user: user, password: password, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="typedmail"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	var p postmarkInbound
	raw, err = stripNUL(raw)
	if err == nil {
		err = json.Unmarshal(raw, &p)
	}
	if err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	msg, err := toInboundMessage(p, raw)
	if err != nil {
		// Redelivering the same payload can't fix it, and 403 is the one
		// non-2xx code that makes Postmark stop retrying.
		h.log.Warn("rejected inbound message", "err", err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	id, created, err := h.saver.SaveInbound(r.Context(), msg)
	switch {
	case errors.Is(err, store.ErrUnknownInbox):
		// Answer 200 anyway: redelivery can't fix an unknown address, and a
		// non-2xx would only make the provider retry it.
		h.log.Warn("dropped message for unknown inbox", "inbox", msg.InboxSlug, "message_id", msg.MessageID)
		writeJSON(w, map[string]any{"status": "dropped"})
	case err != nil:
		h.log.Error("save inbound message", "err", err, "message_id", msg.MessageID)
		http.Error(w, "internal error", http.StatusInternalServerError)
	case !created:
		h.log.Info("duplicate message ignored", "inbox", msg.InboxSlug, "message_id", msg.MessageID)
		writeJSON(w, map[string]any{"status": "duplicate"})
	default:
		h.log.Info("message accepted", "id", id, "inbox", msg.InboxSlug, "message_id", msg.MessageID)
		writeJSON(w, map[string]any{"status": "accepted", "id": id})
	}
}

func (h *Handler) authorized(r *http.Request) bool {
	user, password, ok := r.BasicAuth()
	return ok &&
		subtle.ConstantTimeCompare([]byte(user), []byte(h.user)) == 1 &&
		subtle.ConstantTimeCompare([]byte(password), []byte(h.password)) == 1
}

func toInboundMessage(p postmarkInbound, raw []byte) (store.InboundMessage, error) {
	if p.From == "" {
		return store.InboundMessage{}, errors.New("missing From")
	}
	slug := inboxSlug(p)
	if slug == "" {
		return store.InboundMessage{}, errors.New("cannot determine inbox from recipient")
	}
	id := messageID(p)
	if id == "" {
		return store.InboundMessage{}, errors.New("missing Message-ID")
	}
	return store.InboundMessage{
		InboxSlug:   slug,
		MessageID:   id,
		FromAddress: p.From,
		Subject:     p.Subject,
		TextBody:    p.TextBody,
		HTMLBody:    p.HtmlBody,
		Raw:         raw,
	}, nil
}

// inboxSlug takes the inbox from the "+hash" part of Postmark's inbound
// address, or from the local part of the recipient when a custom inbound
// domain is used (orders@mail.example.com → "orders").
func inboxSlug(p postmarkInbound) string {
	if p.MailboxHash != "" {
		return strings.ToLower(p.MailboxHash)
	}
	local, _, found := strings.Cut(p.OriginalRecipient, "@")
	if !found {
		return ""
	}
	return strings.ToLower(local)
}

// messageID prefers the sender's RFC 5322 Message-ID, which stays the same if
// the same email is sent twice, over Postmark's ID, which is only stable across
// Postmark's own retries. Postmark's ID is prefixed with "postmark:" so it
// can't collide with another provider's IDs or with a sender's Message-ID.
func messageID(p postmarkInbound) string {
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, "Message-ID") && strings.TrimSpace(h.Value) != "" {
			return strings.TrimSpace(h.Value)
		}
	}
	if p.MessageID == "" {
		return ""
	}
	return "postmark:" + p.MessageID
}

// stripNUL removes NUL characters from every string in a JSON payload.
// Postgres rejects them in text and jsonb, so a message containing one could
// never be saved, and every retry from the provider would fail the same way.
// Payloads without a \u0000 escape are returned untouched, so the stored raw
// payload stays verbatim in the common case.
func stripNUL(raw []byte) ([]byte, error) {
	if !bytes.Contains(raw, []byte(`\u0000`)) {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep numbers exactly as sent
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(dropNUL(v)); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func dropNUL(v any) any {
	switch v := v.(type) {
	case string:
		return strings.ReplaceAll(v, "\x00", "")
	case []any:
		for i := range v {
			v[i] = dropNUL(v[i])
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[strings.ReplaceAll(k, "\x00", "")] = dropNUL(x)
		}
		return out
	default:
		return v
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
