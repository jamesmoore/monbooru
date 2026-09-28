package web

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/monbooru/monbooru/internal/logx"
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
	mount := pluginMountBase(name)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, mount)
			pr.Out.URL.RawPath = strings.TrimPrefix(pr.In.URL.EscapedPath(), mount)
			pr.SetURL(target)
			pr.SetXForwarded()
			// The secret the peer minted at pairing is what tells it the
			// call is ours.
			pr.Out.Header.Set("Authorization", "Bearer "+p.PeerToken)
			pr.Out.Header.Set("X-Monbooru-Plugin-Base", mount)
			// Its rights are its token's, not the operator's session.
			pr.Out.Header.Del("Cookie")
		},
		ModifyResponse: func(resp *http.Response) error {
			relocateToMount(resp, base, mount)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.peers.MarkDown(name)
			logx.Warnf("plugin %s: serving %s: %v", name, r.URL.Path, err)
			http.Error(w, "plugin "+name+" did not answer", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

func relocateToMount(resp *http.Response, base, mount string) {
	loc := resp.Header.Get("Location")
	switch {
	case loc == "":
	case strings.HasPrefix(loc, base):
		resp.Header.Set("Location", mount+strings.TrimPrefix(loc, base))
	case strings.HasPrefix(loc, "/"):
		resp.Header.Set("Location", mount+loc)
	}
}
