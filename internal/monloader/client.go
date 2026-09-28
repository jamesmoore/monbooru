// Package monloader is the client for the paired monloader instance.
package monloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// App is the peer's name in pairing records and error text.
const App = "monloader"

// A backstop for callers with an unbounded context: it must stay above every
// per-call deadline, or it cuts off a send monloader may still commit.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// ErrUnconfigured is returned before any I/O when no link is set up.
var ErrUnconfigured = errors.New("monloader is not configured")

// Client reads Base and Token on every call so a re-pair or a pause lands
// without rebuilding it. Base must return "" while the link is paused.
type Client struct {
	Base  func() string
	Token func() string
}

func (c *Client) Do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	base := strings.TrimRight(c.Base(), "/")
	token := c.Token()
	if base == "" || token == "" {
		return nil, ErrUnconfigured
	}
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return httpClient.Do(req)
}

func (c *Client) Post(ctx context.Context, path string, payload map[string]any) (*http.Response, error) {
	body, _ := json.Marshal(payload)
	return c.Do(ctx, http.MethodPost, path, body)
}
