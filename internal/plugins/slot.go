package plugins

import (
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
)

type SlotButton struct {
	Label, Mode, Href string
	Index             int
}

// Off peers still render their buttons, disabled, with OffState saying why.
type SlotPeer struct {
	Name     string
	Off      bool
	OffState string
	Buttons  []SlotButton
}

// The peers offering a button in slot, in name order, with each open
// button's href through mount.
func (p *Peers) Slot(c *config.Config, slot string, imageID int64, fileType, gallery, back string, mount func(name string) string) []SlotPeer {
	blocks := slices.Clone(c.Plugins)
	slices.SortFunc(blocks, func(a, b config.PluginConfig) int { return strings.Compare(a.Name, b.Name) })
	var out []SlotPeer
	for _, pc := range blocks {
		// The address, not the base: a paused peer renders inert rather
		// than vanishing, so a pause reads as a pause.
		if Address(c, pc) == "" {
			continue
		}
		sp := SlotPeer{Name: pc.Name, Off: !p.Usable(pc)}
		if sp.Off {
			sp.OffState = OffState(pc)
		}
		for i, b := range pc.Buttons {
			if b.Slot != slot {
				continue
			}
			if fileType != "" && !b.AppliesTo(fileType) {
				continue
			}
			v := SlotButton{Label: b.Label, Mode: b.Mode, Index: i}
			if b.Mode == config.ModeOpen {
				// Through monbooru's mount, not the paired address: that
				// one is reachable from the server, not the browser.
				v.Href = SubstituteVars(mount(pc.Name)+b.Path, imageID, gallery, back)
			}
			sp.Buttons = append(sp.Buttons, v)
		}
		if len(sp.Buttons) > 0 {
			out = append(out, sp)
		}
	}
	return out
}

// Walks config.PluginVars, the list a button's path is validated
// against, so no declared variable reaches the browser unsubstituted.
func SubstituteVars(target string, imageID int64, gallery, backURL string) string {
	values := map[string]string{
		config.PluginVarImageID: strconv.FormatInt(imageID, 10),
		config.PluginVarGallery: gallery,
		config.PluginVarBackURL: backURL,
	}
	pairs := make([]string, 0, 2*len(config.PluginVars))
	for _, v := range config.PluginVars {
		pairs = append(pairs, v, url.QueryEscape(values[v]))
	}
	return strings.NewReplacer(pairs...).Replace(target)
}

func OffState(p config.PluginConfig) string {
	if p.Paused {
		return "paused"
	}
	return "not responding"
}
