// Package resend fetches received emails from Resend's API. Resend's
// email.received webhook carries metadata only, so the body comes from here.
package resend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const defaultBaseURL = "https://api.resend.com"

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

func New(apiKey string) *Client {
	return &Client{apiKey: apiKey, baseURL: defaultBaseURL, http: http.DefaultClient}
}

// WithBaseURL points the client elsewhere, for tests.
func (c *Client) WithBaseURL(u string) *Client {
	c.baseURL = strings.TrimSuffix(u, "/")
	return c
}

// StatusError is a non-2xx answer from the API.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("resend API: %d %s", e.Status, e.Body)
}

// Permanent reports whether asking again can't help: the email doesn't exist
// or the request is malformed. Authentication errors aren't permanent; they
// are fixed by configuring the key.
func (e *StatusError) Permanent() bool {
	return e.Status == 400 || e.Status == 404 || e.Status == 422
}

// Body is a received email's content.
type Body struct {
	Text string
	HTML string
}

// ReceivedEmail fetches the body of a received email by its email_id.
// https://resend.com/docs/api-reference/emails/retrieve-received-email
func (c *Client) ReceivedEmail(ctx context.Context, id string) (Body, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/emails/receiving/"+url.PathEscape(id), nil)
	if err != nil {
		return Body{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return Body{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return Body{}, err
	}
	if resp.StatusCode/100 != 2 {
		return Body{}, &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}

	var e struct {
		Text *string
		HTML *string
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return Body{}, fmt.Errorf("decode received email: %w", err)
	}
	var body Body
	if e.Text != nil {
		body.Text = *e.Text
	}
	if e.HTML != nil {
		body.HTML, err = decodeDataURI(*e.HTML)
		if err != nil {
			return Body{}, fmt.Errorf("decode html: %w", err)
		}
	}
	return body, nil
}

// decodeDataURI returns the content of a data: URI, which Resend may use for
// HTML (html_format "data_uri"). Anything else is returned as is.
func decodeDataURI(s string) (string, error) {
	if !strings.HasPrefix(s, "data:") {
		return s, nil
	}
	meta, data, ok := strings.Cut(strings.TrimPrefix(s, "data:"), ",")
	if !ok {
		return "", fmt.Errorf("data URI without a comma")
	}
	if strings.HasSuffix(meta, ";base64") {
		b, err := base64.StdEncoding.DecodeString(data)
		return string(b), err
	}
	return url.PathUnescape(data)
}
