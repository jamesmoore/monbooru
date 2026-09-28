package web

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/api"
	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
)

const (
	pairPendingTTL = 5 * time.Minute
	pairMaxPending = 16
)

type pairState string

const (
	pairPending  pairState = "pending"
	pairApproved pairState = "approved"
	pairDenied   pairState = "denied"
)

// Nothing is issued until the claim, so an approval the peer never
// collects mints no token.
type pairReq struct {
	ID        string
	App       string
	URL       string
	Source    string
	Scopes    []string
	PeerToken string
	Version   string
	Buttons   []config.PluginButton
	State     pairState
	Claimed   bool
	CreatedAt time.Time
	Repair    bool
}

type pairStore struct {
	mu sync.Mutex
	m  map[string]*pairReq
}

func newPairStore() *pairStore { return &pairStore{m: map[string]*pairReq{}} }

func pairID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (ps *pairStore) sweepLocked() {
	cutoff := time.Now().Add(-pairPendingTTL)
	maps.DeleteFunc(ps.m, func(_ string, r *pairReq) bool {
		return r.CreatedAt.Before(cutoff)
	})
}

// A second request from the same app replaces its pending one: the
// endpoint is unauthenticated, and one noisy peer would otherwise fill
// the cap for the TTL.
func (ps *pairStore) create(req pairReq) (string, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.sweepLocked()
	maps.DeleteFunc(ps.m, func(_ string, r *pairReq) bool {
		return r.State == pairPending && r.App == req.App
	})
	pending := 0
	for _, r := range ps.m {
		if r.State == pairPending {
			pending++
		}
	}
	if pending >= pairMaxPending {
		return "", false
	}
	req.ID, req.State, req.CreatedAt = pairID(), pairPending, time.Now()
	ps.m[req.ID] = &req
	return req.ID, true
}

// A plugin stopped by a removal may offer to pair on its way out; a card
// for it is noise.
func (ps *pairStore) dropPending(app string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	maps.DeleteFunc(ps.m, func(_ string, r *pairReq) bool {
		return r.State == pairPending && r.App == app
	})
}

func (ps *pairStore) listPending() []pairReq {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.sweepLocked()
	var out []pairReq
	for _, r := range ps.m {
		if r.State == pairPending {
			out = append(out, *r)
		}
	}
	return out
}

func (ps *pairStore) get(id string) (pairReq, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if r, ok := ps.m[id]; ok {
		return *r, true
	}
	return pairReq{}, false
}

func (ps *pairStore) setState(id string, st pairState) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r, ok := ps.m[id]
	if !ok || r.State != pairPending {
		return false
	}
	r.State = st
	return true
}

func (ps *pairStore) claim(id string) (pairReq, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r, ok := ps.m[id]
	if !ok || r.State != pairApproved || r.Claimed {
		return pairReq{}, false
	}
	r.Claimed = true
	return *r, true
}

func (ps *pairStore) unclaim(id string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if r, ok := ps.m[id]; ok {
		r.Claimed = false
	}
}

func (ps *pairStore) remove(id string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.m, id)
}

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
	if err := validatePairOffer(body.App, body.Version, body.Buttons); err != nil {
		api.WriteJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "error": err.Error()})
		return
	}
	// A re-pair is not a conflict: a replaced plugin comes back with no
	// credentials. It still queues for approval.
	repair := s.pairedWith(body.App)
	id, ok := s.pairs.create(pairReq{
		App: body.App, URL: body.URL, Source: clientIP(r), Scopes: grantedScopes(body.RequestedScopes),
		PeerToken: body.PeerToken, Version: body.Version, Buttons: body.Buttons, Repair: repair,
	})
	if !ok {
		api.WriteJSON(w, http.StatusTooManyRequests, map[string]string{"code": "too_many_requests", "error": "too many pending pairing requests"})
		return
	}
	logx.Infof("pairing: request from %s (%s)", body.App, body.URL)
	api.WriteJSON(w, http.StatusOK, map[string]string{"request_id": id, "status": "pending"})
}

func validatePairOffer(app, version string, buttons []config.PluginButton) error {
	if err := config.ValidatePluginName(app); err != nil {
		return err
	}
	if utf8.RuneCountInString(version) > config.MaxPluginVersion {
		return fmt.Errorf("version must be at most %d characters", config.MaxPluginVersion)
	}
	return config.ValidatePluginButtons(buttons)
}

func (s *Server) pairStatus(w http.ResponseWriter, r *http.Request) {
	if !s.pairCORS(w, r) {
		return
	}
	id := r.URL.Query().Get("id")
	req, ok := s.pairs.get(id)
	if !ok {
		api.WriteJSON(w, http.StatusNotFound, map[string]string{"code": "not_found", "error": "unknown pairing request"})
		return
	}
	if req.State != pairApproved {
		api.WriteJSON(w, http.StatusOK, map[string]string{"status": string(req.State)})
		return
	}
	claimed, won := s.pairs.claim(id)
	if !won {
		api.WriteJSON(w, http.StatusOK, map[string]string{"status": "approved"})
		return
	}
	secret, err := s.mintPairedToken(claimed)
	if err != nil {
		s.pairs.unclaim(id)
		api.WriteJSON(w, http.StatusInternalServerError, map[string]string{"code": "mint_failed", "error": err.Error()})
		return
	}
	s.pairs.remove(id)
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

// A peer advertises its own base_url, whose host (usually localhost) means
// nothing from here; the request's source is where it can be reached.
func peerCallbackURL(advertised, source string) string {
	source = strings.TrimSpace(source)
	u, err := url.Parse(strings.TrimSpace(advertised))
	if err != nil || u.Host == "" || source == "" {
		return advertised
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(source, port)
	} else {
		u.Host = source
	}
	return u.String()
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
	moved := peerCallbackURL(was, source)
	if app == "" || moved == was {
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		if t := c.FindPairedToken(app); t != nil {
			t.PeerURL = moved
		}
		return nil
	}); err == nil {
		logx.Infof("pairing: %s moved to %s", app, moved)
	}
}

// The request stores the grant, so the approval card shows what the
// token will carry.
func grantedScopes(requested []string) []string {
	if scopes := filterScopes(requested); len(scopes) > 0 {
		return scopes
	}
	return []string{config.ScopeRead, config.ScopeWrite}
}

func (s *Server) mintPairedToken(req pairReq) (string, error) {
	tok, secret := config.GenerateToken(req.App+" (paired)", grantedScopes(req.Scopes))
	tok.Paired = req.App
	tok.PeerURL = peerCallbackURL(req.URL, req.Source)
	if err := s.withConfig(func(c *config.Config) error {
		// A re-pair replaces the credentials: the copy that held the old
		// token is gone.
		c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t config.Token) bool { return t.Paired == req.App })
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		if req.App == monloaderApp {
			c.Monloader.APIToken = req.PeerToken
			return nil
		}
		p := c.FindPlugin(req.App)
		if p == nil {
			c.Plugins = append(c.Plugins, config.PluginConfig{Name: req.App})
			p = &c.Plugins[len(c.Plugins)-1]
		}
		p.Version, p.PeerToken, p.Buttons, p.Paused = req.Version, req.PeerToken, req.Buttons, false
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
	req, ok := s.pairs.get(id)
	if !ok {
		s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData(r))
		return
	}
	// A peer monbooru cannot reach would pair dead, so approval waits
	// until the url it will call answers.
	base := cmp.Or(s.peerOverrideURL(req.App), peerCallbackURL(req.URL, req.Source))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if !s.monloaderReachable(ctx, base) {
		msg := "no api url to reach " + req.App + "; pairing not completed."
		if base != "" {
			msg = req.App + " is unreachable at " + base + "; pairing not completed. Check the api url and that it is running."
		}
		s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData(r))
		writeFlashOOB(w, "flash-plugins", "warn", msg)
		return
	}
	s.pairs.setState(id, pairApproved)
	logx.Infof("pairing: approved request %s from %s", id, clientIP(r))
	s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData(r))
	writeFlashOOB(w, "flash-plugins", "", "")
}

func (s *Server) monloaderLightDisconnect(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if s.pairedWith(monloaderApp) {
		if err := s.setMonloaderPaused(true); err != nil {
			logx.Errorf("pairing: pause failed: %v", err)
		}
		logx.Infof("pairing: monloader link paused from %s", clientIP(r))
	}
	s.renderMonloaderLight(w, r, "paused", "")
}

func (s *Server) monloaderLightReconnect(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if err := s.setMonloaderPaused(false); err != nil {
		logx.Errorf("pairing: resume failed: %v", err)
	}
	logx.Infof("pairing: monloader link resumed from %s", clientIP(r))
	s.renderMonloaderLight(w, r, "", "")
}

func (s *Server) setMonloaderPaused(paused bool) error {
	return s.withConfig(func(c *config.Config) error {
		c.Monloader.Paused = paused
		return nil
	})
}

func (s *Server) renderMonloaderLight(w http.ResponseWriter, r *http.Request, conn, version string) {
	s.renderTemplate(w, "partials/monloader_light.html", map[string]any{
		"MonloaderConn":    conn,
		"MonloaderVersion": version,
		"MonloaderURL":     s.monloaderWebBase(),
		"CSRFToken":        s.csrfToken(sessionFromContext(r.Context())),
	})
}

func (s *Server) teardownMonloaderPairing(r *http.Request) error {
	peerURL := s.monloaderAPIBase()
	s.cfgMu.RLock()
	peerToken := s.cfg.Monloader.APIToken
	s.cfgMu.RUnlock()
	if err := s.removePairing(monloaderApp); err != nil {
		logx.Errorf("pairing: remove failed: %v", err)
	}
	notifyErr := notifyPeerTeardown(monloaderApp, peerURL, peerToken)
	if notifyErr != nil {
		logx.Errorf("pairing: could not notify monloader of teardown: %v", notifyErr)
	}
	logx.Infof("pairing: removed monloader pairing from %s", clientIP(r))
	return notifyErr
}

// Keeps the configured api_url so an operator's URL survives an unpair
// and re-pair.
func (s *Server) removePairing(app string) error {
	return s.withConfig(func(c *config.Config) error {
		c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t config.Token) bool { return t.Paired == app })
		if app == monloaderApp {
			c.Monloader.APIToken = ""
			return nil
		}
		if p := c.FindPlugin(app); p != nil {
			p.PeerToken, p.Version, p.Buttons = "", "", nil
		}
		// A block with the operator's own lines stays, since config.Save
		// re-encodes the whole file from the struct. Enabled counts: dropping
		// it would leave a running plugin whose row offers to enable it.
		c.Plugins = slices.DeleteFunc(c.Plugins, func(p config.PluginConfig) bool {
			return p.Name == app && p.APIURL == "" && !p.Enabled
		})
		return nil
	})
}
