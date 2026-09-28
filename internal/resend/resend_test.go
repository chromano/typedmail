package resend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(t *testing.T, status int, body string) (*Client, *http.Request) {
	t.Helper()
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New("re_test").WithBaseURL(srv.URL), &got
}

func TestReceivedEmail(t *testing.T) {
	c, req := serve(t, 200, `{"object": "email", "id": "abc", "text": "Hello", "html": "<p>Hello</p>", "html_format": "raw"}`)

	body, err := c.ReceivedEmail(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if body.Text != "Hello" || body.HTML != "<p>Hello</p>" {
		t.Errorf("body = %+v", body)
	}
	if req.URL.Path != "/emails/receiving/abc" || req.Header.Get("Authorization") != "Bearer re_test" {
		t.Errorf("request = %s %s", req.URL.Path, req.Header.Get("Authorization"))
	}
}

func TestReceivedEmailDecodesDataURIAndNulls(t *testing.T) {
	// "<p>Hi</p>" base64-encoded, as with html_format "data_uri".
	c, _ := serve(t, 200, `{"text": null, "html": "data:text/html;base64,PHA+SGk8L3A+", "html_format": "data_uri"}`)

	body, err := c.ReceivedEmail(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if body.Text != "" || body.HTML != "<p>Hi</p>" {
		t.Errorf("body = %+v", body)
	}
}

func TestDecodeDataURI(t *testing.T) {
	tests := map[string]string{
		"<p>plain</p>":                                     "<p>plain</p>",
		"data:text/html,%3Cb%3Ehi%3C/b%3E":                 "<b>hi</b>",
		"data:text/html;charset=utf-8;base64,PGI+aGk8L2I+": "<b>hi</b>",
	}
	for in, want := range tests {
		got, err := decodeDataURI(in)
		if err != nil || got != want {
			t.Errorf("decodeDataURI(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestReceivedEmailErrors(t *testing.T) {
	tests := []struct {
		status    int
		permanent bool
	}{
		{404, true},
		{422, true},
		{401, false},
		{429, false},
		{500, false},
	}
	for _, tt := range tests {
		c, _ := serve(t, tt.status, `{"message": "nope"}`)
		_, err := c.ReceivedEmail(context.Background(), "abc")
		var se *StatusError
		if !errors.As(err, &se) || se.Status != tt.status || se.Permanent() != tt.permanent {
			t.Errorf("status %d: err = %v, want permanent=%v", tt.status, err, tt.permanent)
		}
	}
}
