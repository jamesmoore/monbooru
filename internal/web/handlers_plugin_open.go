package web

import (
	"net/http"
	"net/url"
)

const pluginMountPrefix = "/plugins/"

func pluginMountBase(name string) string { return pluginMountPrefix + name }

// Pages sit one prefix deep: X-Monbooru-Plugin-Base lets them build their
// own links, since a root-relative one would land on monbooru's routes.
func (s *Server) pluginMount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, ok := s.plugin(name)
	base := s.pluginBase(p)
	if !ok || p.PeerToken == "" || base == "" {
		s.renderNotFound(w, r)
		return
	}
	target, err := url.Parse(base)
	if err != nil {
		s.renderNotFound(w, r)
		return
	}
	s.peers.MountProxy(r, name, target, base, p.PeerToken, pluginMountBase(name)).ServeHTTP(w, r)
}
