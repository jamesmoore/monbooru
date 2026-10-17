package plugins

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/monbooru/monbooru/internal/shared/logx"
)

// A transport failure marks the peer down; a browser that went away
// does not.
func (p *Peers) MountProxy(r *http.Request, name string, target *url.URL, base, token, mount string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, mount)
			pr.Out.URL.RawPath = strings.TrimPrefix(pr.In.URL.EscapedPath(), mount)
			pr.SetURL(target)
			pr.SetXForwarded()
			// The secret the peer minted at pairing is what tells it the
			// call is ours.
			pr.Out.Header.Set("Authorization", "Bearer "+token)
			pr.Out.Header.Set("X-Monbooru-Plugin-Base", mount)
			// Its rights are its token's, not the operator's session.
			pr.Out.Header.Del("Cookie")
		},
		ModifyResponse: func(resp *http.Response) error {
			relocateToMount(resp, base, mount)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			// A browser that went away cancels the outbound call too.
			if req.Context().Err() == nil {
				p.MarkDown(name)
				logx.Warnf("plugin %s: serving %s: %v", name, r.URL.Path, err)
			}
			http.Error(w, "plugin "+name+" did not answer", http.StatusBadGateway)
		},
	}
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
