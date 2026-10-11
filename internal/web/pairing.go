package web

import (
	"cmp"
	"context"
	"encoding/json"
	"github.com/monbooru/monbooru/internal/monloader"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/api"
	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/peer"
)

// The pairing routes sit outside the api auth wrapper, since the operator's
// approval is the gate, so they apply its CORS policy themselves.
func (s *Server) pairCORS(w http.ResponseWriter, r *http.Request) bool {
	if api.SetCORS(w, r, s.cfgSnapshot()) {
		return true
	}
	api.WriteJSON(w, http.StatusForbidden, map[string]string{"code": "forbidden", "error": "CORS: origin not allowed"})
	return false
}

func (s *Server) pairedWith(app string) bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.FindPairedToken(app) != nil
}

func (s *Server) pairRequest(w http.ResponseWriter, r *http.Request) {
	if !s.pairCORS(w, r) {
		return
	}
	var body struct {
		App             string                `json:"app"`
		URL             string                `json:"url"`
		RequestedScopes []string              `json:"requested_scopes"`
		PeerToken       string                `json:"peer_token"`
		Version         string                `json:"version"`
		Buttons         []config.PluginButton `json:"buttons"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.App == "" {
		api.WriteJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "error": "app and a JSON body are required"})
		return
	}
	if err := peer.ValidateOffer(body.App, body.Version, body.Buttons); err != nil {
		api.WriteJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "error": err.Error()})
		return
	}
	// A re-pair is not a conflict: a replaced plugin comes back with no
	// credentials. It still queues for approval.
	repair := s.pairedWith(body.App)
	id, ok := s.pairs.Create(peer.Req{
		App: body.App, URL: body.URL, Source: clientIP(r), Scopes: peer.GrantedScopes(body.RequestedScopes),
		PeerToken: body.PeerToken, Version: body.Version, Buttons: body.Buttons, Repair: repair,
	})
	if !ok {
		api.WriteJSON(w, http.StatusTooManyRequests, map[string]string{"code": "too_many_requests", "error": "too many pending pairing requests"})
		return
	}
	logx.Infof("pairing: request from %s (%s)", body.App, body.URL)
	api.WriteJSON(w, http.StatusOK, map[string]string{"request_id": id, "status": "pending"})
}

func (s *Server) pairStatus(w http.ResponseWriter, r *http.Request) {
	if !s.pairCORS(w, r) {
		return
	}
	id := r.URL.Query().Get("id")
	req, ok := s.pairs.Get(id)
	if !ok {
		api.WriteJSON(w, http.StatusNotFound, map[string]string{"code": "not_found", "error": "unknown pairing request"})
		return
	}
	if req.State != peer.Approved {
		api.WriteJSON(w, http.StatusOK, map[string]string{"status": string(req.State)})
		return
	}
	claimed, won := s.pairs.Claim(id)
	if !won {
		api.WriteJSON(w, http.StatusOK, map[string]string{"status": "approved"})
		return
	}
	secret, err := s.mintPairedToken(claimed)
	if err != nil {
		s.pairs.Unclaim(id)
		api.WriteJSON(w, http.StatusInternalServerError, map[string]string{"code": "mint_failed", "error": err.Error()})
		return
	}
	s.pairs.Remove(id)
	api.WriteJSON(w, http.StatusOK, map[string]string{"status": "approved", "token": secret})
}

// Removes only locally: calling the peer back would loop.
func (s *Server) pairTeardown(w http.ResponseWriter, r *http.Request) {
	if !s.pairCORS(w, r) {
		return
	}
	secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.cfgMu.RLock()
	tok := s.cfg.FindTokenByHash(config.HashToken(secret))
	var paired string
	if tok != nil {
		paired = tok.Paired
	}
	s.cfgMu.RUnlock()
	if secret == "" || paired == "" {
		api.WriteJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized", "error": "pairing token required"})
		return
	}
	if err := s.removePairing(paired); err != nil {
		api.WriteJSON(w, http.StatusInternalServerError, map[string]string{"code": "remove_failed", "error": err.Error()})
		return
	}
	logx.Infof("pairing: %s removed the pairing remotely", paired)
	api.WriteJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// Any call carrying a peer's own token names where it is now, so a
// recreated container's new IP is followed without a re-pair.
func (s *Server) notePeerAddress(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			s.repointPeer(secret, clientIP(r))
		}
		next(w, r)
	}
}

func (s *Server) repointPeer(secret, source string) {
	s.cfgMu.RLock()
	var app, was string
	if tok := s.cfg.FindTokenByHash(config.HashToken(secret)); tok != nil {
		app, was = tok.Paired, tok.PeerURL
	}
	s.cfgMu.RUnlock()
	moved := peer.CallbackURL(was, source)
	if app == "" || moved == was {
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		peer.Repoint(c, app, moved)
		return nil
	}); err == nil {
		logx.Infof("pairing: %s moved to %s", app, moved)
	}
}

func (s *Server) mintPairedToken(req peer.Req) (string, error) {
	tok, secret := config.GenerateToken(req.App+" (paired)", peer.GrantedScopes(req.Scopes))
	tok.Paired = req.App
	tok.PeerURL = peer.CallbackURL(req.URL, req.Source)
	if err := s.withConfig(func(c *config.Config) error {
		peer.Grant(c, req, tok)
		return nil
	}); err != nil {
		return "", err
	}
	logx.Infof("pairing: issued token to %s", req.App)
	return secret, nil
}

func (s *Server) pluginPairApprove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	id := r.PathValue("id")
	req, ok := s.pairs.Get(id)
	if !ok {
		s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData(r))
		return
	}
	// A peer monbooru cannot reach would pair dead, so approval waits
	// until the url it will call answers.
	base := cmp.Or(s.peerOverrideURL(req.App), peer.CallbackURL(req.URL, req.Source))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if !peer.Reachable(ctx, base) {
		msg := "no api url to reach " + req.App + "; pairing not completed."
		if base != "" {
			msg = req.App + " is unreachable at " + base + "; pairing not completed. Check the api url and that it is running."
		}
		s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData(r))
		writeFlashOOB(w, "flash-plugins", "warn", msg)
		return
	}
	s.pairs.SetState(id, peer.Approved)
	logx.Infof("pairing: approved request %s from %s", id, clientIP(r))
	s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData(r))
	writeFlashOOB(w, "flash-plugins", "", "")
}

func (s *Server) monloaderLightDisconnect(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if s.pairedWith(monloader.App) {
		if err := s.setPluginPaused(monloader.App, true); err != nil {
			logx.Errorf("pairing: pause failed: %v", err)
		}
		logx.Infof("pairing: monloader link paused from %s", clientIP(r))
	}
	s.renderMonloaderLight(w, r, peer.ConnPaused, "")
}

func (s *Server) monloaderLightReconnect(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if err := s.setPluginPaused(monloader.App, false); err != nil {
		logx.Errorf("pairing: resume failed: %v", err)
	}
	logx.Infof("pairing: monloader link resumed from %s", clientIP(r))
	s.renderMonloaderLight(w, r, "", "")
}

func (s *Server) renderMonloaderLight(w http.ResponseWriter, r *http.Request, conn, version string) {
	s.renderTemplate(w, "partials/monloader_light.html", map[string]any{
		"MonloaderConn":    conn,
		"MonloaderVersion": version,
		"MonloaderSeen":    r.FormValue("seen"),
		"MonloaderURL":     s.monloaderWebBase(),
	})
}

func (s *Server) teardownMonloaderPairing(r *http.Request) error {
	notifyErr := s.ml.Teardown(func() {
		if err := s.removePairing(monloader.App); err != nil {
			logx.Errorf("pairing: remove failed: %v", err)
		}
	})
	logx.Infof("pairing: removed monloader pairing from %s", clientIP(r))
	return notifyErr
}

func (s *Server) removePairing(app string) error {
	return s.withConfig(func(c *config.Config) error {
		peer.Revoke(c, app)
		return nil
	})
}
