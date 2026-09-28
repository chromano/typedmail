package ingest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/chromano/typedmail/internal/store"
)

type fakeSaver struct {
	got     []store.InboundMessage
	created bool
	err     error
}

func (f *fakeSaver) SaveInbound(_ context.Context, m store.InboundMessage) (int64, bool, error) {
	f.got = append(f.got, m)
	return 1, f.created, f.err
}

func samplePayload(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("../../testdata/postmark_inbound.json")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func post(t *testing.T, h http.Handler, body any, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case string:
		r = strings.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		r = strings.NewReader(string(j))
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/postmark", r)
	if auth {
		req.SetBasicAuth("postmark", "secret")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newTestHandler(s Saver) *Handler {
	return NewHandler(s, "postmark", "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestHandler(t *testing.T) {
	tests := []struct {
		name       string
		auth       bool
		body       func(map[string]any) any
		saver      fakeSaver
		wantCode   int
		wantStatus string // "status" field of the JSON response, when 200
		wantSaved  bool
	}{
		{
			name:     "accepts a new message",
			auth:     true,
			body:     func(p map[string]any) any { return p },
			saver:    fakeSaver{created: true},
			wantCode: http.StatusOK, wantStatus: "accepted", wantSaved: true,
		},
		{
			name:     "acknowledges a duplicate",
			auth:     true,
			body:     func(p map[string]any) any { return p },
			saver:    fakeSaver{created: false},
			wantCode: http.StatusOK, wantStatus: "duplicate", wantSaved: true,
		},
		{
			name:     "drops unknown inbox with 200 so the provider doesn't retry",
			auth:     true,
			body:     func(p map[string]any) any { return p },
			saver:    fakeSaver{err: store.ErrUnknownInbox},
			wantCode: http.StatusOK, wantStatus: "dropped", wantSaved: true,
		},
		{
			name:     "rejects missing credentials",
			auth:     false,
			body:     func(p map[string]any) any { return p },
			wantCode: http.StatusUnauthorized,
		},
		{
			name:     "rejects invalid JSON",
			auth:     true,
			body:     func(map[string]any) any { return "{not json" },
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "forbids a message without From so the provider doesn't retry",
			auth:     true,
			body:     func(p map[string]any) any { delete(p, "From"); return p },
			wantCode: http.StatusForbidden,
		},
		{
			name: "forbids a message without any Message-ID",
			auth: true,
			body: func(p map[string]any) any {
				p["Headers"] = []any{}
				delete(p, "MessageID")
				return p
			},
			wantCode: http.StatusForbidden,
		},
		{
			name: "forbids a message whose recipient names no inbox",
			auth: true,
			body: func(p map[string]any) any {
				p["MailboxHash"] = ""
				p["OriginalRecipient"] = "not-an-address"
				return p
			},
			wantCode: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saver := tt.saver
			rec := post(t, newTestHandler(&saver), tt.body(samplePayload(t)), tt.auth)

			if rec.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantStatus != "" {
				var resp struct{ Status string }
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp.Status != tt.wantStatus {
					t.Errorf("status = %q, want %q", resp.Status, tt.wantStatus)
				}
			}
			if saved := len(saver.got) > 0; saved != tt.wantSaved {
				t.Errorf("saved = %v, want %v", saved, tt.wantSaved)
			}
		})
	}
}

func TestMapping(t *testing.T) {
	tests := []struct {
		name          string
		edit          func(map[string]any)
		wantSlug      string
		wantMessageID string
	}{
		{
			name:          "uses mailbox hash and RFC Message-ID header",
			edit:          func(map[string]any) {},
			wantSlug:      "orders",
			wantMessageID: "<CAF=po10442.8f1b@mail.acme-retail.com>",
		},
		{
			name: "falls back to recipient local part on a custom domain",
			edit: func(p map[string]any) {
				p["MailboxHash"] = ""
				p["OriginalRecipient"] = "Invoices@mail.example.com"
			},
			wantSlug:      "invoices",
			wantMessageID: "<CAF=po10442.8f1b@mail.acme-retail.com>",
		},
		{
			name:          "falls back to Postmark's ID without a Message-ID header",
			edit:          func(p map[string]any) { p["Headers"] = []any{} },
			wantSlug:      "orders",
			wantMessageID: "postmark:73e6d360-66eb-11e1-8e72-a8904824019b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := samplePayload(t)
			tt.edit(p)
			saver := fakeSaver{created: true}
			if rec := post(t, newTestHandler(&saver), p, true); rec.Code != http.StatusOK {
				t.Fatalf("code = %d (body %q)", rec.Code, rec.Body.String())
			}

			got := saver.got[0]
			if got.InboxSlug != tt.wantSlug {
				t.Errorf("slug = %q, want %q", got.InboxSlug, tt.wantSlug)
			}
			if got.MessageID != tt.wantMessageID {
				t.Errorf("message ID = %q, want %q", got.MessageID, tt.wantMessageID)
			}
			if got.FromAddress != "buyer@acme-retail.com" || got.Subject != "PO 10442 - restock" {
				t.Errorf("unexpected mapping: %+v", got)
			}
			if !json.Valid(got.Raw) {
				t.Error("raw payload is not valid JSON")
			}
		})
	}
}

func TestHandlerStripsNULBytes(t *testing.T) {
	p := samplePayload(t)
	p["Subject"] = "PO\x00 10442"
	p["TextBody"] = "Please ship\x00 200 stickers."
	p["Headers"] = []any{map[string]any{"Name": "Message-ID", "Value": "<po10442\x00@mail.acme-retail.com>"}}
	p["X-Extra\x00"] = []any{"a\x00b", 1.5}

	var saver fakeSaver
	saver.created = true
	rec := post(t, newTestHandler(&saver), p, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(saver.got) != 1 {
		t.Fatalf("saved %d messages, want 1", len(saver.got))
	}
	got := saver.got[0]
	if got.Subject != "PO 10442" {
		t.Errorf("subject = %q, want %q", got.Subject, "PO 10442")
	}
	if got.TextBody != "Please ship 200 stickers." {
		t.Errorf("text body = %q", got.TextBody)
	}
	if got.MessageID != "<po10442@mail.acme-retail.com>" {
		t.Errorf("message ID = %q", got.MessageID)
	}
	if strings.Contains(string(got.Raw), `\u0000`) || strings.ContainsRune(string(got.Raw), 0) {
		t.Errorf("raw payload still contains NUL: %s", got.Raw)
	}
	var raw map[string]any
	if err := json.Unmarshal(got.Raw, &raw); err != nil {
		t.Fatalf("raw payload is not valid JSON: %v", err)
	}
	if extra, ok := raw["X-Extra"].([]any); !ok || extra[0] != "ab" || extra[1] != 1.5 {
		t.Errorf("X-Extra = %v, want [ab 1.5]", raw["X-Extra"])
	}
}

func TestStripNUL(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"leaves a payload without NUL verbatim", `{"b": 1, "a": "<x>"}`, `{"b": 1, "a": "<x>"}`},
		{"keeps an escaped backslash followed by u0000", `{"a":"\\u0000"}`, `{"a":"\\u0000"}`},
		{"keeps numbers as sent", `{"n":1.50,"s":"\u0000"}`, `{"n":1.50,"s":""}`},
		{"does not escape HTML", `{"s":"<b>\u0000&"}`, `{"s":"<b>&"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := stripNUL([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("stripNUL(%s) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}
