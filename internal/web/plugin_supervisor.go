package web

import (
	"net"
)

// The listener, not server.base_url: behind a proxy base_url may not
// answer from inside the child's container, and the child shares the
// listener's host.
func (s *Server) pluginCallbackURL() string {
	s.cfgMu.RLock()
	addr, base := s.cfg.Server.BindAddress, s.cfg.Server.BaseURL
	s.cfgMu.RUnlock()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return base
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func (s *Server) startManagedPlugins() {
	for _, p := range s.effectivePlugins() {
		if !p.Installed {
			continue
		}
		if !p.Enabled {
			// A cold probe cache reads as up, so without this a disabled
			// plugin's buttons return on every restart.
			s.peers.MarkDown(p.Name)
			continue
		}
		s.peers.Start(p.Launch)
	}
}
