package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// A backstop for callers with an unbounded context: it must stay above every
// per-call deadline, or it cuts off a send the peer may still commit.
var Client = &http.Client{Timeout: 15 * time.Second}

// A refused request, not a sign the peer is down, so a batch skips the
// row. It carries the peer's name: a plugin's refusal must not read as
// monloader's.
type StatusError struct{ Peer, Status string }

func (e StatusError) Error() string { return e.Peer + " returned " + e.Status }

func IsStatusError(err error) bool {
	var se StatusError
	return errors.As(err, &se)
}

// APIVersion moves on any change to /api/v1 a paired peer would trip
// over: a path or field gone, a status code or a vocabulary word changed.
// MinPeerAPI is the oldest peer API this side still drives.
const (
	APIVersion = 2
	MinPeerAPI = 1
)

type HealthInfo struct {
	Version    string `json:"version"`
	API        int    `json:"api"`
	MinPeerAPI int    `json:"min_peer_api"`
}

// APIMismatch is empty when the two sides agree, and when the peer
// predates the field.
func (h HealthInfo) APIMismatch() string {
	if h.API == 0 || (h.API >= MinPeerAPI && h.MinPeerAPI <= APIVersion) {
		return ""
	}
	return fmt.Sprintf("api mismatch: it speaks api %d and drives %d or newer, this monbooru speaks %d and drives %d or newer",
		h.API, h.MinPeerAPI, APIVersion, MinPeerAPI)
}

// A 200 with any body passes; every field is optional.
func Health(ctx context.Context, base string) (HealthInfo, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/health", nil)
	if err != nil {
		return HealthInfo{}, false
	}
	resp, err := Client.Do(req)
	if err != nil {
		return HealthInfo{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return HealthInfo{}, false
	}
	var h HealthInfo
	_ = json.NewDecoder(resp.Body).Decode(&h)
	return h, true
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
	resp, err := Client.Do(req)
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
