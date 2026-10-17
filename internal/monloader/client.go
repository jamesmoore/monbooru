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

	"github.com/monbooru/monbooru/internal/peer"
)

// App is the peer's name in pairing records and error text.
const App = peer.Companion

// errUnconfigured is returned before any I/O when no link is set up.
var errUnconfigured = errors.New("monloader is not configured")

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
		return nil, errUnconfigured
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
	return peer.Client.Do(req)
}

func (c *Client) Post(ctx context.Context, path string, payload map[string]any) (*http.Response, error) {
	body, _ := json.Marshal(payload)
	return c.Do(ctx, http.MethodPost, path, body)
}
