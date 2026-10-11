package web

import (
	"context"
	"fmt"
	"net/http"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/models"
)

// Honours the ladder and the budget, so it cannot be leaned on to burn a
// day's quota.
func (s *Server) lookupDuePost(w http.ResponseWriter, r *http.Request) {
	cx := s.active()
	if cx == nil {
		writeInlineFlash(w, "err", "no active gallery")
		return
	}
	if !s.ml.Usable() {
		writeInlineFlash(w, "err", "monloader is unreachable")
		return
	}
	s.cfgMu.RLock()
	sched := s.cfg.Schedule
	s.cfgMu.RUnlock()
	if !sched.LookupPTR && !sched.LookupBooru {
		writeInlineFlash(w, "err", "no lookup is enabled in Settings > Schedule")
		return
	}
	ptrOn := sched.RunsOn(config.ActionLookupPTR, cx.Name)
	booruOn := sched.RunsOn(config.ActionLookupBooru, cx.Name)
	if !ptrOn && !booruOn {
		writeInlineFlash(w, "err", fmt.Sprintf("lookups are off for gallery %q in Settings > Schedule", cx.Name))
		return
	}
	s.goJob(w, models.JobTypeLookup, "Lookup started. Watch the status bar.", func(ctx context.Context) {
		s.ml.LookupDue(ctx, cx, ptrOn, booruOn)
	})
}
