package web

import (
	"context"
	"net/http"
	"time"

	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/peer"
)

func (s *Server) monloaderStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !s.pairedWith(monloader.App) {
		// Stop polling and clear the light once the pairing is gone.
		_, _ = w.Write([]byte(`<span id="monloader-light"></span>`))
		return
	}
	if s.ml.Paused() {
		s.renderMonloaderLight(w, r, peer.ConnPaused, "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st := s.ml.Refresh(ctx)
	// Panels seeded from stale flags re-mount on this event. Compared with
	// what the page was last told, not with the cache before the probe: the
	// background probe refreshes the cache too.
	seen := s.ml.Status().Panels()
	if r.FormValue("seen") != seen {
		w.Header().Set("HX-Trigger", "monloader-status-changed")
	}
	s.renderTemplate(w, "partials/monloader_light.html", map[string]any{
		"MonloaderConn":     st.Conn,
		"MonloaderVersion":  st.Version,
		"MonloaderMismatch": st.APIMismatch,
		"MonloaderSeen":     seen,
		"MonloaderURL":      s.monloaderWebBase(),
	})
}
