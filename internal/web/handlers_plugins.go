package web

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
)

type pluginRowView struct {
	Name       string
	Version    string
	Address    string
	Conn       string
	Buttons    []config.PluginButton
	TokenName  string
	Scopes     []string
	Paused     bool
	Paired     bool
	Companion  bool
	APIURL     string
	WebURL     string
	WebURLHint string
	Command    string
	RunState   string
	// Installed is a plugin found in the plugins folder: its Enable
	// button is the operator's consent to a launch line they never typed.
	Installed   bool
	Enabled     bool
	SelfManaged bool
}

func (s *Server) pluginRows() []pluginRowView {
	var managed, paired []pluginRowView
	for _, p := range s.effectivePlugins() {
		// Residue, typically of a folder removed after being enabled: the
		// block stays but earns no row.
		if p.PeerToken == "" && !p.Installed && p.APIURL == "" {
			continue
		}
		row := pluginRowView{
			Name:      p.Name,
			Version:   s.pluginVersion(p.PluginConfig),
			Address:   s.pluginAddress(p.PluginConfig),
			Buttons:   p.Buttons,
			Paused:    p.Paused,
			Installed: p.Installed,
			Enabled:   p.Enabled,
		}
		if p.PeerToken != "" {
			row.Conn = peerConn(p.Paused, s.peers.ProbeSeed(p.Name).Conn)
			row.TokenName, row.Scopes = s.pairedTokenInfo(p.Name)
			row.Paired = true
		}
		if p.Installed {
			row.Command = commandLine(p)
			row.RunState = s.peers.State(p.Name)
			managed = append(managed, row)
			continue
		}
		row.SelfManaged = true
		paired = append(paired, row)
	}
	byName := func(a, b pluginRowView) int { return strings.Compare(a.Name, b.Name) }
	slices.SortFunc(managed, byName)
	slices.SortFunc(paired, byName)

	// Rendered even unpaired: its api url is an override an operator may
	// set before pairing.
	rows := make([]pluginRowView, 0, len(managed)+len(paired)+1)
	rows = append(rows, s.monloaderRow())
	rows = append(rows, managed...)
	return append(rows, paired...)
}

func (s *Server) monloaderRow() pluginRowView {
	ml := s.mlStatus.Seed()
	s.cfgMu.RLock()
	apiURL, webURL := s.cfg.Monloader.APIURL, s.cfg.Server.MonloaderURL
	paused := s.cfg.Monloader.Paused
	s.cfgMu.RUnlock()
	name, scopes := s.pairedTokenInfo(monloaderApp)
	return pluginRowView{
		Name:       monloaderApp,
		Version:    ml.Version,
		Address:    s.monloaderAPIBase(),
		Conn:       peerConn(paused, ml.Conn),
		TokenName:  name,
		Scopes:     scopes,
		Paused:     paused,
		Paired:     s.pairedWith(monloaderApp),
		Companion:  true,
		APIURL:     apiURL,
		WebURL:     webURL,
		WebURLHint: "http://localhost:8456",
	}
}

func commandLine(p effectivePlugin) string {
	return strings.TrimSpace(p.Launch.Command + " " + strings.Join(p.Launch.Args, " "))
}

func peerConn(paused bool, probe string) string {
	if paused {
		return "paused"
	}
	return cmp.Or(probe, "checking")
}

func (s *Server) pairedTokenInfo(app string) (string, []string) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if t := s.cfg.FindPairedToken(app); t != nil {
		return t.Name, t.Scopes
	}
	return "", nil
}

func (s *Server) pluginVersion(p config.PluginConfig) string {
	return cmp.Or(s.peers.ProbeSeed(p.Name).Version, p.Version)
}

func (s *Server) pairedPeerCount() int {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	n := 0
	for _, t := range s.cfg.Auth.Tokens {
		if t.Paired != "" {
			n++
		}
	}
	return n
}

func (s *Server) pairViewData(r *http.Request) map[string]any {
	return map[string]any{
		"Pending":   s.pairs.listPending(),
		"Paired":    s.pairedPeerCount(),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	}
}

func (s *Server) pluginPairingFragment(w http.ResponseWriter, r *http.Request) {
	data := s.pairViewData(r)
	count, _ := data["Paired"].(int)
	s.renderTemplate(w, "partials/plugin_pairing.html", data)
	if was := r.URL.Query().Get("paired"); was != "" && was != strconv.Itoa(count) {
		s.renderPluginRows(w, r, true)
		s.renderAuthTokensOOB(w, r)
	}
}

func (s *Server) pluginPairDeny(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.pairs.setState(r.PathValue("id"), pairDenied)
	s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData(r))
}

func (s *Server) pluginPairRemove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	var notifyErr error
	if name == monloaderApp {
		notifyErr = s.teardownMonloaderPairing(r)
	} else {
		notifyErr = s.teardownPluginPairing(name)
		logx.Infof("pairing: removed %s pairing from %s", name, clientIP(r))
		if p, ok := s.effective(name); ok && p.Installed {
			// A plugin left running would offer to pair again a moment
			// later and undo the click.
			if p.Enabled {
				s.setPluginEnabled(name, false)
				s.peers.Stop(name)
				s.peers.MarkDown(name)
				s.pairs.dropPending(name)
			}
			// No flash: a folder has no far end to clean up by hand, and
			// the token is revoked either way, so a copy the plugin kept
			// authenticates to nothing.
			notifyErr = nil
		}
	}
	s.renderPluginRows(w, r, false)
	s.renderAuthTokensOOB(w, r)
	if notifyErr != nil {
		writeFlashOOB(w, "flash-plugins", "warn", "Removed here, but could not reach "+name+" - remove the pairing there too.")
	}
}

func (s *Server) pluginPause(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	paused := r.FormValue("paused") == "1"
	if err := s.setPluginPaused(name, paused); err != nil {
		logx.Errorf("plugins: pause %s: %v", name, err)
	}
	logx.Infof("plugins: %s %s from %s", name, map[bool]string{true: "paused", false: "resumed"}[paused], clientIP(r))
	s.renderPluginRows(w, r, false)
}

// Start and stop both drop the cached probe so the dot and buttons follow
// the click, not the next probe.
func (s *Server) pluginStart(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if p, ok := s.effective(r.PathValue("name")); ok && p.Installed {
		s.setPluginEnabled(p.Name, true)
		s.peers.Start(p.Launch)
		s.peers.ClearProbe(p.Name)
		logx.Infof("plugins: started %s from %s", p.Name, clientIP(r))
	}
	s.renderPluginRows(w, r, false)
}

func (s *Server) pluginStop(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	if p, ok := s.effective(name); ok && p.Installed {
		s.setPluginEnabled(name, false)
	}
	s.peers.Stop(name)
	s.peers.MarkDown(name)
	logx.Infof("plugins: stopped %s from %s", name, clientIP(r))
	s.renderPluginRows(w, r, false)
}

func (s *Server) setPluginEnabled(name string, enabled bool) {
	err := s.withConfig(func(c *config.Config) error {
		if p := c.FindPlugin(name); p != nil {
			p.Enabled = enabled
		} else if enabled {
			c.Plugins = append(c.Plugins, config.PluginConfig{Name: name, Enabled: true})
		}
		return nil
	})
	if err != nil {
		logx.Errorf("plugins: persist enabled=%v for %s: %v", enabled, name, err)
	}
}

func (s *Server) renderPluginRows(w http.ResponseWriter, r *http.Request, oob bool) {
	s.renderTemplate(w, "partials/plugin_rows.html", map[string]any{
		"Rows":      s.pluginRows(),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
		"OOB":       oob,
	})
}

func (s *Server) setPluginPaused(name string, paused bool) error {
	if name == monloaderApp {
		return s.setMonloaderPaused(paused)
	}
	return s.withConfig(func(c *config.Config) error {
		if p := c.FindPlugin(name); p != nil {
			p.Paused = paused
		}
		return nil
	})
}

func (s *Server) teardownPluginPairing(name string) error {
	p, ok := s.plugin(name)
	if !ok {
		return nil
	}
	// Read the address before removePairing drops the token it may live on.
	base, token := s.pluginAddress(p), p.PeerToken
	if err := s.removePairing(name); err != nil {
		logx.Errorf("pairing: remove %s failed: %v", name, err)
	}
	s.peers.ClearProbe(name)
	notifyErr := notifyPeerTeardown(name, base, token)
	if notifyErr != nil {
		logx.Warnf("pairing: could not notify %s of teardown: %v", name, notifyErr)
	}
	return notifyErr
}
