package ingest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	svix "github.com/svix/svix-webhooks/go"

	"github.com/chromano/typedmail/internal/store"
)

var resendSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("test-secret-for-resend-webhooks!"))

func resendPayload(t *testing.T, edit func(map[string]any)) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/resend_received.json")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(p)
	}
	b, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// postResend signs body as Resend would at the given time and posts it.
func postResend(t *testing.T, h http.Handler, body []byte, signedAt time.Time, secret string) *httptest.ResponseRecorder {
	t.Helper()
	wh, err := svix.NewWebhook(secret)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := wh.Sign("msg_1", signedAt, body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/resend", bytes.NewReader(body))
	req.Header.Set("svix-id", "msg_1")
	req.Header.Set("svix-timestamp", strconv.FormatInt(signedAt.Unix(), 10))
	req.Header.Set("svix-signature", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newResendHandler(t *testing.T, s Saver) *ResendHandler {
	t.Helper()
	h, err := NewResendHandler(s, resendSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestResendHandler(t *testing.T) {
	other := "whsec_" + base64.StdEncoding.EncodeToString([]byte("a-different-secret-entirely-xyz"))
	tests := []struct {
		name       string
		edit       func(map[string]any)
		signedAt   time.Time
		secret     string
		saver      fakeSaver
		wantCode   int
		wantStatus string
		wantSaved  bool
	}{
		{name: "accepts a received email", saver: fakeSaver{created: true},
			wantCode: http.StatusOK, wantStatus: "accepted", wantSaved: true},
		{name: "acknowledges a duplicate", saver: fakeSaver{created: false},
			wantCode: http.StatusOK, wantStatus: "duplicate", wantSaved: true},
		{name: "drops unknown inbox", saver: fakeSaver{err: store.ErrUnknownInbox},
			wantCode: http.StatusOK, wantStatus: "dropped", wantSaved: true},
		{name: "rejects a wrong signature", secret: other, wantCode: http.StatusUnauthorized},
		{name: "rejects a replayed old request", signedAt: time.Now().Add(-time.Hour), wantCode: http.StatusUnauthorized},
		{name: "ignores other event types", edit: func(p map[string]any) { p["type"] = "email.delivered" },
			wantCode: http.StatusOK, wantStatus: "ignored"},
		{name: "rejects an email without recipients",
			edit:     func(p map[string]any) { p["data"].(map[string]any)["to"] = []any{} },
			wantCode: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.signedAt.IsZero() {
				tt.signedAt = time.Now()
			}
			if tt.secret == "" {
				tt.secret = resendSecret
			}
			saver := tt.saver
			rec := postResend(t, newResendHandler(t, &saver), resendPayload(t, tt.edit), tt.signedAt, tt.secret)

			if rec.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantStatus != "" {
				var resp struct{ Status string }
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Status != tt.wantStatus {
					t.Errorf("status = %q (%v), want %q", resp.Status, err, tt.wantStatus)
				}
			}
			if saved := len(saver.got) > 0; saved != tt.wantSaved {
				t.Errorf("saved = %v, want %v", saved, tt.wantSaved)
			}
		})
	}
}

func TestResendMapping(t *testing.T) {
	tests := []struct {
		name          string
		edit          func(d map[string]any)
		wantSlug      string
		wantMessageID string
	}{
		{"uses the first recipient's local part and the sender's Message-ID", nil,
			"orders", "<CAF=po10517.2c4d@mail.acme-retail.com>"},
		{"accepts a named recipient on a custom domain",
			func(d map[string]any) { d["to"] = []any{"Invoices <Invoices@mail.example.com>"} },
			"invoices", "<CAF=po10517.2c4d@mail.acme-retail.com>"},
		{"uses the +tag when there is one",
			func(d map[string]any) { d["to"] = []any{"support+Invoices@mail.example.com"} },
			"invoices", "<CAF=po10517.2c4d@mail.acme-retail.com>"},
		{"ignores an empty +tag",
			func(d map[string]any) { d["to"] = []any{"orders+@k7x2.resend.app"} },
			"orders+", "<CAF=po10517.2c4d@mail.acme-retail.com>"},
		{"falls back to Resend's ID without a Message-ID",
			func(d map[string]any) { d["message_id"] = "" },
			"orders", "resend:56761188-7520-42d8-8898-ff6fc54ce618"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saver := fakeSaver{created: true}
			body := resendPayload(t, func(p map[string]any) {
				if tt.edit != nil {
					tt.edit(p["data"].(map[string]any))
				}
			})
			rec := postResend(t, newResendHandler(t, &saver), body, time.Now(), resendSecret)
			if rec.Code != http.StatusOK || len(saver.got) != 1 {
				t.Fatalf("code = %d, saved %d (body %q)", rec.Code, len(saver.got), rec.Body.String())
			}
			got := saver.got[0]
			if got.InboxSlug != tt.wantSlug || got.MessageID != tt.wantMessageID {
				t.Errorf("slug, message ID = %q, %q; want %q, %q", got.InboxSlug, got.MessageID, tt.wantSlug, tt.wantMessageID)
			}
			if got.Provider != store.ProviderResend || got.ProviderID != "56761188-7520-42d8-8898-ff6fc54ce618" ||
				got.FromAddress != "buyer@acme-retail.com" || got.TextBody != "" {
				t.Errorf("message = %+v", got)
			}
		})
	}
}
