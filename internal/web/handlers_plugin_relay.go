package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
)

const (
	// Bumps only on a breaking change to pluginRelayRequest.
	pluginPayloadVersion = 1
	// No retry: the peer may have committed the work, and a second call
	// would repeat it.
	pluginRelayTimeout = 10 * time.Second
	pluginMessageMax   = 200
)

type pluginRelayRequest struct {
	Payload  int     `json:"payload"`
	Monbooru string  `json:"monbooru"`
	Gallery  string  `json:"gallery"`
	Slot     string  `json:"slot"`
	Button   string  `json:"button"`
	ImageIDs []int64 `json:"image_ids"`
}

// A 204 with a flash, not an error status: the click swaps nothing, and
// htmx would discard an error body unseen.
func relayRefused(w http.ResponseWriter, msg string) {
	setFlashHeader(w, msg, "err", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pluginRelay(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) || pageGalleryStale(w, r, s.activeGallery()) {
		return
	}
	name := r.FormValue("plugin")
	p, ok := s.plugin(name)
	if !ok || !s.pluginUsable(p) {
		relayRefused(w, "plugin "+name+" is not available")
		return
	}
	idx, err := strconv.Atoi(r.FormValue("button"))
	if err != nil || idx < 0 || idx >= len(p.Buttons) || p.Buttons[idx].Mode != config.ModeRelay {
		relayRefused(w, "unknown plugin button")
		return
	}
	ids := parseIDList(r.Form["ids"])
	if len(ids) == 0 {
		relayRefused(w, "no images selected")
		return
	}
	button := p.Buttons[idx]
	scoped := s.scopeForButton(button, ids)
	if len(scoped) == 0 {
		relayRefused(w, "nothing selected that "+name+" handles")
		return
	}
	// The peer never hears of the rows its media excluded, so its message
	// cannot account for them.
	var narrowed string
	if n := len(ids) - len(scoped); n > 0 {
		narrowed = fmt.Sprintf(" (%d of %d sent; %d not handled by %s)", len(scoped), len(ids), n, name)
	}
	ids = scoped
	base := s.pluginBase(p)
	if base == "" || p.PeerToken == "" {
		relayRefused(w, "plugin "+name+" has no address to call")
		return
	}

	// The route is gallery-free so the peer call never runs under ctxMu,
	// hence the snapshot.
	galleryName := s.activeGallery()

	answer, err := s.callPluginRelay(r.Context(), p.Name, base+button.Path, p.PeerToken, pluginRelayRequest{
		Payload:  pluginPayloadVersion,
		Monbooru: Version,
		Gallery:  galleryName,
		Slot:     button.Slot,
		Button:   button.Label,
		ImageIDs: ids,
	})
	if err != nil {
		s.peers.MarkDown(name)
		logx.Warnf("plugin relay %s: %v", name, err)
		relayRefused(w, "plugin "+name+" did not answer")
		return
	}
	message := truncateRunes(answer.Message, pluginMessageMax)
	if !answer.OK {
		if message == "" {
			message = "plugin " + name + " refused the request"
		}
		relayRefused(w, message+narrowed)
		return
	}
	setFlashHeader(w, message+narrowed, "ok", nil)
	if answer.Refresh {
		w.Header().Set("HX-Refresh", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) scopeForButton(b config.PluginButton, ids []int64) []int64 {
	if b.Media == "" {
		return ids
	}
	d := s.db()
	if d == nil {
		return ids
	}
	handled := make(map[int64]bool, len(ids))
	err := db.Chunked(ids, 500, func(chunk []int64) error {
		in, args := db.InPlaceholders(chunk)
		rows, err := d.Read.Query(`SELECT id, file_type FROM images WHERE id IN (`+in+`)`, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			var fileType string
			if err := rows.Scan(&id, &fileType); err == nil && b.AppliesTo(fileType) {
				handled[id] = true
			}
		}
		return rows.Err()
	})
	// A partial read knows nothing of the rows it never reached; dropping
	// them would quietly shrink the scope.
	if err != nil {
		logx.Warnf("plugin scope for %s: %v", b.Label, err)
		return ids
	}
	return slices.DeleteFunc(ids, func(id int64) bool { return !handled[id] })
}

type pluginRelayAnswer struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Refresh bool   `json:"refresh"`
}

func (s *Server) callPluginRelay(ctx context.Context, peer, target, token string, payload pluginRelayRequest) (pluginRelayAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, pluginRelayTimeout)
	defer cancel()
	body, err := json.Marshal(payload)
	if err != nil {
		return pluginRelayAnswer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return pluginRelayAnswer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := pluginClient.Do(req)
	if err != nil {
		return pluginRelayAnswer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return pluginRelayAnswer{}, peerStatusError{peer, resp.Status}
	}
	var answer pluginRelayAnswer
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<16)).Decode(&answer); err != nil {
		return pluginRelayAnswer{}, err
	}
	return answer, nil
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
