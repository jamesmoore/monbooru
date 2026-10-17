package plugins

import (
	"cmp"
	"context"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

type Probe struct {
	Conn      string // "" until the first probe, then peer.ConnOK or ConnDown
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
	p.SetProbe(name, Probe{Conn: peer.ConnDown, CheckedAt: time.Now()})
}

func (p *Peers) Stale(name string) bool {
	return time.Since(p.ProbeSeed(name).CheckedAt) >= ProbeTTL
}

const (
	ProbeInterval = 30 * time.Second
	ProbeTimeout  = 4 * time.Second
)

// A cold probe cache counts as up so a fresh boot does not blank the buttons.
func (p *Peers) Usable(pc config.PluginConfig) bool {
	if pc.Paused {
		return false
	}
	return p.ProbeSeed(pc.Name).Conn != peer.ConnDown
}

func (p *Peers) Version(pc config.PluginConfig) string {
	return cmp.Or(p.ProbeSeed(pc.Name).Version, pc.Version)
}

func (p *Peers) StartManaged(list []Effective) {
	for _, e := range list {
		if !e.Installed {
			continue
		}
		if !e.Enabled {
			// A cold probe cache reads as up, so without this a disabled
			// plugin's buttons return on every restart.
			p.MarkDown(e.Name)
			continue
		}
		p.Start(e.Launch)
	}
}

func (p *Peers) Refresh(ctx context.Context, c *config.Config) {
	var wg sync.WaitGroup
	for _, pc := range c.Plugins {
		if pc.PeerToken == "" || pc.Paused {
			continue
		}
		if !p.Stale(pc.Name) {
			continue
		}
		base := Base(c, pc)
		if base == "" {
			p.MarkDown(pc.Name)
			continue
		}
		wg.Go(func() {
			h, ok := peer.Health(ctx, base)
			if !ok {
				p.MarkDown(pc.Name)
				return
			}
			p.SetProbe(pc.Name, Probe{Conn: peer.ConnOK, Version: h.Version, CheckedAt: time.Now()})
		})
	}
	wg.Wait()
}

// Start and stop both drop the cached probe so the dot and buttons follow
// the click, not the next probe.
func (p *Peers) Run(e Effective) {
	p.Start(e.Launch)
	p.ClearProbe(e.Name)
}

func (p *Peers) Halt(name string) {
	p.Stop(name)
	p.MarkDown(name)
}

// A plugin left running would offer to pair again a moment later and
// undo the removal, so its pending offer goes after the stop.
func (p *Peers) Retire(pairs *peer.Store, name string) {
	p.Halt(name)
	pairs.DropPending(name)
}

func (p *Peers) Teardown(name, base, token string) error {
	p.ClearProbe(name)
	notifyErr := peer.NotifyTeardown(name, base, token)
	if notifyErr != nil {
		logx.Warnf("pairing: could not notify %s of teardown: %v", name, notifyErr)
	}
	return notifyErr
}
