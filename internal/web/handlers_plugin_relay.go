package web

import (
	"cmp"
	"fmt"
	"net/http"
	"strconv"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/plugins"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// A 204 with a flash, not an error status: the click swaps nothing, and
// htmx would discard an error body unseen.
func relayRefused(w http.ResponseWriter, msg string) {
	setFlashHeader(w, msg, "err", nil)
	w.WriteHeader(http.StatusNoContent)
}

type relayPlan struct {
	plugin   config.PluginConfig
	button   config.PluginButton
	base     string
	gallery  string
	ids      []int64
	narrowed string
}

func (s *Server) pluginRelay(w http.ResponseWriter, r *http.Request) {
	plan, ok := s.planRelay(w, r)
	if !ok {
		return
	}
	name := plan.plugin.Name
	answer, err := plugins.CallRelay(r.Context(), name, plan.base+plan.button.Path, plan.plugin.PeerToken, plugins.RelayRequest{
		Payload:  plugins.PayloadVersion,
		Monbooru: Version,
		Gallery:  plan.gallery,
		Slot:     plan.button.Slot,
		Button:   plan.button.Label,
		ImageIDs: plan.ids,
	})
	switch {
	case err != nil && r.Context().Err() != nil:
		return
	case peer.IsStatusError(err):
		logx.Warnf("plugin relay %s: %v", name, err)
		relayRefused(w, err.Error())
		return
	case err != nil:
		s.peers.MarkDown(name)
		logx.Warnf("plugin relay %s: %v", name, err)
		relayRefused(w, "plugin "+name+" did not answer")
		return
	}
	message := truncateRunes(answer.Message, plugins.MessageMax)
	if !answer.OK {
		relayRefused(w, cmp.Or(message, "plugin "+name+" refused the request")+plan.narrowed)
		return
	}
	setFlashHeader(w, message+plan.narrowed, "ok", nil)
	if answer.Refresh {
		w.Header().Set("HX-Refresh", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

// The route is gallery-free so the peer call never runs under the set's
// lock; the plan holds it so the stale check, the narrowing and the
// payload's gallery all read one gallery.
func (s *Server) planRelay(w http.ResponseWriter, r *http.Request) (*relayPlan, bool) {
	s.set.RLock()
	defer s.set.RUnlock()
	galleryName := s.activeGallery()
	if !parseFormOK(w, r) || pageGalleryStale(w, r, galleryName) {
		return nil, false
	}
	name := r.FormValue("plugin")
	p, ok := s.plugin(name)
	if !ok || !s.peers.Usable(p) {
		relayRefused(w, "plugin "+name+" is not available")
		return nil, false
	}
	idx, err := strconv.Atoi(r.FormValue("button"))
	if err != nil || idx < 0 || idx >= len(p.Buttons) || p.Buttons[idx].Mode != config.ModeRelay {
		relayRefused(w, "unknown plugin button")
		return nil, false
	}
	ids := parseIDList(r.Form["ids"])
	if len(ids) == 0 {
		relayRefused(w, "no images selected")
		return nil, false
	}
	button := p.Buttons[idx]
	scoped := s.scopeForButton(button, ids)
	if len(scoped) == 0 {
		relayRefused(w, "nothing selected that "+name+" handles")
		return nil, false
	}
	// The peer never hears of the rows its media excluded, so its message
	// cannot account for them.
	var narrowed string
	if n := len(ids) - len(scoped); n > 0 {
		narrowed = fmt.Sprintf(" (%d of %d sent; %d not handled by %s)", len(scoped), len(ids), n, name)
	}
	base := s.pluginBase(p)
	if base == "" || p.PeerToken == "" {
		relayRefused(w, "plugin "+name+" has no address to call")
		return nil, false
	}
	return &relayPlan{plugin: p, button: button, base: base, gallery: galleryName, ids: scoped, narrowed: narrowed}, true
}

func (s *Server) scopeForButton(b config.PluginButton, ids []int64) []int64 {
	d := s.db()
	if d == nil {
		return ids
	}
	return plugins.Scope(b, ids, func(ids []int64) (map[int64]string, error) { return gallery.FileTypes(d, ids) })
}

// Byte length bounds rune count, so a short string skips the conversion.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
