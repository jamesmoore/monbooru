package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/plugins"
)

const monloaderApp = "monloader"

// A backstop only: it must stay above every per-call deadline.
var pluginClient = &http.Client{Timeout: 15 * time.Second}

const (
	pluginProbeInterval = 30 * time.Second
	pluginProbeTimeout  = 4 * time.Second
)

func (s *Server) plugins() []config.PluginConfig {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return slices.Clone(s.cfg.Plugins)
}

func (s *Server) plugin(name string) (config.PluginConfig, bool) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if p := s.cfg.FindPlugin(name); p != nil {
		return *p, true
	}
	return config.PluginConfig{}, false
}

// A paused block answers "", so every outbound call short-circuits.
func (s *Server) pluginBase(p config.PluginConfig) string {
	if p.Paused {
		return ""
	}
	return s.pluginAddress(p)
}

func (s *Server) pluginAddress(p config.PluginConfig) string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if u := strings.TrimSpace(p.APIURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	if t := s.cfg.FindPairedToken(p.Name); t != nil {
		return strings.TrimRight(t.PeerURL, "/")
	}
	return ""
}

// A cold probe cache counts as up so a fresh boot does not blank the buttons.
func (s *Server) pluginUsable(p config.PluginConfig) bool {
	if p.Paused {
		return false
	}
	return s.peers.ProbeSeed(p.Name).Conn != "down"
}

func pluginOffState(p config.PluginConfig) string {
	if p.Paused {
		return "paused"
	}
	return "not responding"
}

func (s *Server) peerOverrideURL(app string) string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if app == monloaderApp {
		return strings.TrimSpace(s.cfg.Monloader.APIURL)
	}
	if p := s.cfg.FindPlugin(app); p != nil {
		return strings.TrimSpace(p.APIURL)
	}
	return ""
}

// A 200 with any body passes; the version is optional.
func probePeer(ctx context.Context, client *http.Client, base string) (string, bool) {
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

func notifyPeerTeardown(app, baseURL, token string) error {
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
	resp, err := pluginClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Any 2xx: the teardown is idempotent, so a 204 has done what was asked.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return peerStatusError{app, resp.Status}
	}
	return nil
}

func (s *Server) refreshPluginProbes(ctx context.Context) {
	var wg sync.WaitGroup
	// The scheduled lookup phases gate on monloader's cached state and
	// run with no page open to poll the light, so without this a
	// monloader down once is skipped every night after.
	if s.pairedWith(monloaderApp) && !s.monloaderPaused() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.monloaderStatusCached(ctx)
		}()
	}
	for _, p := range s.plugins() {
		if p.PeerToken == "" || p.Paused {
			continue
		}
		if !s.peers.Stale(p.Name) {
			continue
		}
		base := s.pluginBase(p)
		if base == "" {
			s.peers.MarkDown(p.Name)
			continue
		}
		wg.Add(1)
		go func(name, base string) {
			defer wg.Done()
			version, ok := probePeer(ctx, pluginClient, base)
			if !ok {
				s.peers.MarkDown(name)
				return
			}
			s.peers.SetProbe(name, plugins.Probe{Conn: "ok", Version: version, CheckedAt: time.Now()})
		}(p.Name, base)
	}
	wg.Wait()
}

func (s *Server) runPluginProbes() {
	ticker := time.NewTicker(pluginProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), pluginProbeTimeout)
			s.refreshPluginProbes(ctx)
			cancel()
		case <-s.done:
			return
		}
	}
}
