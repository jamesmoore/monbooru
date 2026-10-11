package peer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// The connection a probe leaves a peer in; "" is a cold cache.
const (
	ConnOK       = "ok"
	ConnDown     = "down"
	ConnRejected = "rejected"
	ConnPaused   = "paused"
)

// A backstop only: it must stay above every per-call deadline.
var client = &http.Client{Timeout: 15 * time.Second}

// A refused request, not a sign the peer is down, so a batch skips the
// row. It carries the peer's name: a plugin's refusal must not read as
// monloader's.
type StatusError struct{ Peer, Status string }

func (e StatusError) Error() string { return e.Peer + " returned " + e.Status }

func IsStatusError(err error) bool {
	var se StatusError
	return errors.As(err, &se)
}

// A 200 with any body passes; the version is optional.
func Health(ctx context.Context, base string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/health", nil)
	if err != nil {
		return "", false
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var h struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&h)
	return h.Version, true
}

func Reachable(ctx context.Context, base string) bool {
	if strings.TrimRight(base, "/") == "" {
		return false
	}
	_, up := Health(ctx, base)
	return up
}

func NotifyTeardown(app, baseURL, token string) error {
	base := strings.TrimRight(baseURL, "/")
	if base == "" || token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/pair/remove", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Any 2xx: the teardown is idempotent, so a 204 has done what was asked.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return StatusError{app, resp.Status}
	}
	return nil
}
