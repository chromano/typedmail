package extract

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

var orderSchema = json.RawMessage(`{
	"type": "object",
	"required": ["po_number"],
	"properties": {"po_number": {"type": "string"}, "total": {"type": ["string", "null"]}}
}`)

var email = Email{
	From:     "buyer@acme-retail.com",
	Subject:  "PO 10442 - restock",
	TextBody: "Please ship PO 10442. Total: $412.50",
}

// fakeAPI answers every request with status and body, and keeps the last
// request so tests can inspect it.
type fakeAPI struct {
	status int
	body   string
	req    map[string]any
	header http.Header
}

func (f *fakeAPI) serve(t *testing.T) *Extractor {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.req = nil
		_ = json.Unmarshal(b, &f.req)
		f.header = r.Header
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		io.WriteString(w, f.body)
	}))
	t.Cleanup(srv.Close)
	return New("", option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
}

func message(stopReason, content string) string {
	return `{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5",
		"content": ` + content + `, "stop_reason": "` + stopReason + `",
		"stop_details": ` + map[bool]string{true: `{"type": "refusal", "category": "cyber", "explanation": "no"}`, false: "null"}[stopReason == "refusal"] + `,
		"usage": {"input_tokens": 10, "output_tokens": 5}}`
}

func TestExtractSendsSchemaAndEmail(t *testing.T) {
	api := &fakeAPI{status: 200, body: message("end_turn",
		`[{"type": "text", "text": "{\"po_number\": \"10442\", \"total\": \"412.50\"}"}]`)}
	ex := api.serve(t)

	got, err := ex.Extract(context.Background(), orderSchema, email)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"po_number": "10442", "total": "412.50"}` {
		t.Errorf("result = %s", got)
	}

	if api.req["model"] != DefaultModel {
		t.Errorf("model = %v, want %s", api.req["model"], DefaultModel)
	}
	if api.req["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v, want default", api.req["fallbacks"])
	}
	if b := api.header.Get("anthropic-beta"); !strings.Contains(b, "server-side-fallback-2026-07-01") {
		t.Errorf("anthropic-beta = %q", b)
	}
	format := api.req["output_config"].(map[string]any)["format"].(map[string]any)
	total := format["schema"].(map[string]any)["properties"].(map[string]any)["total"].(map[string]any)
	if format["type"] != "json_schema" || total["anyOf"] == nil {
		t.Errorf("format = %v, want json_schema with the rewritten schema", format)
	}
	msgs := api.req["messages"].([]any)
	text := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{email.From, email.Subject, "412.50", "<email>"} {
		if !strings.Contains(text, want) {
			t.Errorf("user message %q doesn't contain %q", text, want)
		}
	}
}

func TestExtractErrors(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		schema        json.RawMessage
		unextractable bool
	}{
		{"refusal", 200, message("refusal", `[]`), orderSchema, true},
		{"output too long", 200, message("max_tokens", `[{"type": "text", "text": "{\"po"}]`), orderSchema, true},
		{"invalid JSON", 200, message("end_turn", `[{"type": "text", "text": "not json"}]`), orderSchema, false},
		{"bad request", 400, `{"type": "error", "error": {"type": "invalid_request_error", "message": "bad schema"}}`, orderSchema, true},
		{"rate limited", 429, `{"type": "error", "error": {"type": "rate_limit_error", "message": "slow down"}}`, orderSchema, false},
		{"overloaded", 529, `{"type": "error", "error": {"type": "overloaded_error", "message": "busy"}}`, orderSchema, false},
		{"bad API key", 401, `{"type": "error", "error": {"type": "authentication_error", "message": "no"}}`, orderSchema, false},
		{"inbox without a schema", 200, message("end_turn", `[]`), json.RawMessage(`{}`), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := (&fakeAPI{status: tt.status, body: tt.body}).serve(t)
			_, err := ex.Extract(context.Background(), tt.schema, email)
			if err == nil {
				t.Fatal("got no error")
			}
			if got := errors.Is(err, ErrUnextractable); got != tt.unextractable {
				t.Errorf("unextractable = %v, want %v (err: %v)", got, tt.unextractable, err)
			}
		})
	}
}

func TestUserPromptFallsBackToHTML(t *testing.T) {
	p := userPrompt(Email{From: "a@b.c", Subject: "s", TextBody: "  ", HTMLBody: "<p>hi</p>"})
	if !strings.Contains(p, "<p>hi</p>") {
		t.Errorf("prompt = %q, want the HTML body", p)
	}
}

// TestLive calls the real API with each sample email and its inbox schema.
// It runs only when ANTHROPIC_API_KEY is set, and costs a few API calls.
func TestLive(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	samples := map[string]string{
		"orders":    "../../testdata/postmark_inbound.json",
		"invoices":  "../../testdata/postmark_invoice.json",
		"shipments": "../../testdata/postmark_shipment.json",
	}
	ex := New(os.Getenv("EXTRACT_MODEL"))
	for inbox, path := range samples {
		t.Run(inbox, func(t *testing.T) {
			schema, err := os.ReadFile("../../schemas/" + inbox + ".json")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var p struct{ From, Subject, TextBody, HtmlBody string }
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
			out, err := ex.Extract(context.Background(), schema, Email{p.From, p.Subject, p.TextBody, p.HtmlBody})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s", out)

			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			var s struct{ Required []string }
			_ = json.Unmarshal(schema, &s)
			for _, k := range s.Required {
				if got[k] == nil {
					t.Errorf("required field %q missing", k)
				}
			}
		})
	}
}
