package web

import (
	"context"
	"github.com/monbooru/monbooru/internal/monloader"
	"slices"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/plugins"
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

func (s *Server) pluginBase(p config.PluginConfig) string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return plugins.Base(s.cfg, p)
}

func (s *Server) pluginCallbackURL() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return plugins.CallbackURL(s.cfg.Server.BindAddress, s.cfg.Server.BaseURL)
}

func (s *Server) pluginAddress(p config.PluginConfig) string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return plugins.Address(s.cfg, p)
}

func (s *Server) peerOverrideURL(app string) string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return peer.OverrideURL(s.cfg, app)
}

func (s *Server) refreshPluginProbes(ctx context.Context) {
	var wg sync.WaitGroup
	// The scheduled lookup phases gate on monloader's cached state and
	// run with no page open to poll the light, so without this a
	// monloader down once is skipped every night after.
	if s.pairedWith(monloader.App) && !s.ml.Paused() {
		wg.Go(func() {
			s.ml.Refresh(ctx)
		})
	}
	s.peers.Refresh(ctx, s.cfgSnapshot())
	wg.Wait()
}

func (s *Server) runPluginProbes() {
	ticker := time.NewTicker(plugins.ProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), plugins.ProbeTimeout)
			s.refreshPluginProbes(ctx)
			cancel()
		case <-s.done:
			return
		}
	}
}
