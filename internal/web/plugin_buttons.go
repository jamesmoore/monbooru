package web

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
)

type pluginButtonView struct {
	Label string
	Mode  string
	Href  string
	Index int
	Off   bool
	Why   string
}

type pluginGroup struct {
	Peer    string
	Buttons []pluginButtonView
}

// ImageID is 0 on the gallery, where a relay acts on the live selection.
type pluginSlotView struct {
	ImageID int64
	Groups  []pluginGroup
}

func (v pluginSlotView) Any() bool { return len(v.Groups) > 0 }

func (v pluginSlotView) AnyOpen() bool {
	for _, g := range v.Groups {
		if slices.ContainsFunc(g.Buttons, func(b pluginButtonView) bool { return b.Mode == config.ModeOpen }) {
			return true
		}
	}
	return false
}

func (s *Server) pluginSlot(r *http.Request, slot string, imageID int64, fileType string) pluginSlotView {
	peers := s.plugins()
	slices.SortFunc(peers, func(a, b config.PluginConfig) int { return strings.Compare(a.Name, b.Name) })
	back, gallery := s.pageURL(r), s.activeGallery()
	view := pluginSlotView{ImageID: imageID}
	for _, p := range peers {
		// pluginAddress, not pluginBase: a paused peer renders inert
		// rather than vanishing, so a pause reads as a pause.
		if s.pluginAddress(p) == "" {
			continue
		}
		off := !s.pluginUsable(p)
		g := pluginGroup{Peer: p.Name}
		for i, b := range p.Buttons {
			if b.Slot != slot {
				continue
			}
			if fileType != "" && !b.AppliesTo(fileType) {
				continue
			}
			v := pluginButtonView{Label: b.Label, Mode: b.Mode, Index: i, Off: off}
			if off {
				v.Why = p.Name + " is " + pluginOffState(p)
			}
			if b.Mode == config.ModeOpen {
				// Through monbooru's mount, not the paired address: that
				// one is reachable from the server, not the browser.
				v.Href = substitutePluginVars(pluginMountBase(p.Name)+b.Path, imageID, gallery, back)
			}
			g.Buttons = append(g.Buttons, v)
		}
		if len(g.Buttons) > 0 {
			view.Groups = append(view.Groups, g)
		}
	}
	return view
}

func substitutePluginVars(target string, imageID int64, gallery, backURL string) string {
	return strings.NewReplacer(
		"{image_id}", url.QueryEscape(strconv.FormatInt(imageID, 10)),
		"{gallery}", url.QueryEscape(gallery),
		"{back_url}", url.QueryEscape(backURL),
	).Replace(target)
}

// The host is the one the browser reached: base_url defaults to
// localhost, which sends a LAN browser nowhere.
func (s *Server) pageURL(r *http.Request) string {
	scheme := "http"
	s.cfgMu.RLock()
	configured := s.cfg.Server.BaseURL
	s.cfgMu.RUnlock()
	if strings.HasPrefix(configured, "https://") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}
