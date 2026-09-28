package plugins

import (
	"sync"
	"time"
)

type Probe struct {
	Conn      string // "" until the first probe, then "ok" or "down"
	Version   string
	CheckedAt time.Time
}

const ProbeTTL = 10 * time.Second

type Peers struct {
	*Supervisor

	mu     sync.Mutex
	probes map[string]Probe
}

func NewPeers(callbackURL func() string, done <-chan struct{}) *Peers {
	return &Peers{Supervisor: New(callbackURL, done), probes: map[string]Probe{}}
}

func (p *Peers) ProbeSeed(name string) Probe {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probes[name]
}

func (p *Peers) ClearProbe(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.probes, name)
}

func (p *Peers) SetProbe(name string, pr Probe) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probes[name] = pr
}

func (p *Peers) MarkDown(name string) {
	p.SetProbe(name, Probe{Conn: "down", CheckedAt: time.Now()})
}

func (p *Peers) Stale(name string) bool {
	return time.Since(p.ProbeSeed(name).CheckedAt) >= ProbeTTL
}
