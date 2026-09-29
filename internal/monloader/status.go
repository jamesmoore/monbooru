package monloader

import (
	"sync"
	"time"
)

type Status struct {
	Conn          string // "" until the first probe, then "ok", "down" or "rejected"
	Version       string
	PTR           bool
	PTRSyncing    bool
	Contrib       bool
	ContribBanned bool
	ContribFailed int
}

type StatusCache struct {
	mu        sync.Mutex
	status    Status
	checkedAt time.Time
}

func (c *StatusCache) Seed() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

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

// Expire exists for the tests; the application never expires a probe early.
func (c *StatusCache) Expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkedAt = time.Time{}
}
