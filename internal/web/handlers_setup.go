package web

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/desktop"
	"github.com/monbooru/monbooru/internal/logx"
)

type setupData struct {
	BooruName string
	CSRFToken string
	// The rest of what partials/head.html reads.
	Title        string
	BooruFavicon string
	Theme        bool
	Err          string
	GalleryPath  string
	LAN          bool
	Menu         bool
	Autostart    bool
	Port         string
	Integration  desktopIntegration
}

func (s *Server) setupPending() bool {
	if !s.desktop {
		return false
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return !s.cfg.SetupDone
}

func (s *Server) setupMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.setupPending() || setupExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if isHTMXRequest(r) {
			w.Header().Set("HX-Redirect", "/setup")
			w.WriteHeader(http.StatusOK)
			return
		}
		// An API client would take the wizard's HTML 200 for an answer,
		// so it gets a JSON refusal.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "monbooru has not been set up yet",
				"code":  "setup_pending",
			})
			return
		}
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	})
}

func setupExempt(path string) bool {
	switch path {
	case "/health", "/setup", "/internal/browse":
		return true
	}
	// With auth on, the session gate sends /setup to /login, so gating
	// the login here would loop.
	return isStaticPath(path) || isPublicPath(path)
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if !s.desktop {
		s.notFoundHandler(w, r)
		return
	}
	s.cfgMu.RLock()
	galleryPath := ""
	if len(s.cfg.Galleries) > 0 {
		galleryPath = s.cfg.Galleries[0].GalleryPath
	}
	bind := s.cfg.Server.BindAddress
	s.cfgMu.RUnlock()
	s.renderSetup(w, r, setupData{
		GalleryPath: galleryPath,
		LAN:         !desktop.IsLoopbackAddr(bind),
		Menu:        true,
		Autostart:   true,
	})
}

func (s *Server) renderSetup(w http.ResponseWriter, r *http.Request, d setupData) {
	s.cfgMu.RLock()
	bind := s.cfg.Server.BindAddress
	s.cfgMu.RUnlock()
	_, port, err := net.SplitHostPort(bind)
	if err != nil {
		port = "8455"
	}
	d.BooruName = s.booruName()
	d.Title = d.BooruName + " setup"
	d.CSRFToken = s.csrfToken(sessionFromContext(r.Context()))
	d.BooruFavicon = s.booruFaviconURL()
	d.Theme = s.activeTheme().Path != ""
	d.Port = port
	d.Integration = s.desktopIntegration()
	s.renderTemplate(w, "setup.html", d)
}

// The gallery goes first, the one input that can fail; SetupDone is saved
// with the address, so a submit that fails midway leaves the wizard up.
func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	if !s.desktop {
		http.NotFound(w, r)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	form := setupData{
		GalleryPath: strings.TrimSpace(r.FormValue("gallery_path")),
		LAN:         r.FormValue("reach") == "lan",
		Menu:        r.FormValue("menu_entry") == "on",
		Autostart:   r.FormValue("start_at_login") == "on",
	}
	if err := s.repointGallery(s.defaultGallery(), form.GalleryPath); err != nil {
		form.Err = err.Error()
		s.renderSetup(w, r, form)
		return
	}
	err := s.withConfig(func(c *config.Config) error {
		_, port, splitErr := net.SplitHostPort(c.Server.BindAddress)
		if splitErr != nil {
			return fmt.Errorf("server.bind_address %q is not a host:port", c.Server.BindAddress)
		}
		host := "127.0.0.1"
		if form.LAN {
			host = "0.0.0.0"
		}
		c.Server.BindAddress = net.JoinHostPort(host, port)
		c.SetupDone = true
		return nil
	})
	if err != nil {
		form.Err = "Could not save: " + err.Error()
		s.renderSetup(w, r, form)
		return
	}
	h := s.desktopHook()
	if h.MenuSupported() {
		if err := h.SetMenu(form.Menu); err != nil {
			logx.Warnf("setup: could not write the menu entry: %v", err)
		}
	}
	if h.AutostartSupported() {
		if err := h.SetAutostart(form.Autostart); err != nil {
			logx.Warnf("setup: could not write start at login: %v", err)
		}
	}
	logx.Infof("setup: done, reachable from %s", map[bool]string{true: "the network", false: "this computer"}[form.LAN])
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
