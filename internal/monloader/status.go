package monloader

import (
	"sync"
	"time"
)

// Status is what one probe of the peer reported. An empty Conn is a cold
// cache, which the footer light renders as "checking" and the PTR surfaces
// read as "not yet".
type Status struct {
	Conn          string // "" | "ok" | "down" | "rejected"
	Version       string
	PTR           bool
	PTRSyncing    bool
	Contrib       bool
	ContribBanned bool
	ContribFailed int
}

// StatusCache holds the last Status a probe produced. Every page's initial
// render seeds from it so the light shows its last known state at once
// instead of flickering back to "checking" - and re-probing - on every
// navigation.
type StatusCache struct {
	mu        sync.Mutex
	status    Status
	checkedAt time.Time
}

// Seed is the cached Status without probing.
func (c *StatusCache) Seed() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Fresh serves the cached Status while it is inside ttl. A cache that has
// never been written is never fresh.
func (c *StatusCache) Fresh(ttl time.Duration) (Status, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.Conn != "" && time.Since(c.checkedAt) < ttl {
		return c.status, true
	}
	return Status{}, false
}

func (c *StatusCache) Store(st Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status, c.checkedAt = st, time.Now()
}

// Expire drops the freshness window without dropping what the cache holds,
// so the next read re-probes. Exported for the transport's tests, which
// reach the window through this rather than by sleeping out the TTL; the
// application never expires a probe early.
func (c *StatusCache) Expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkedAt = time.Time{}
}
