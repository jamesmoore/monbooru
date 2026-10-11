package web

import (
	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/monloader"
)

type schedHost struct{ s *Server }

// Unpaired, the lookup phases are off in this copy only; the saved
// switches wait for the next pairing.
func (h schedHost) View() (config.ScheduleConfig, []string) {
	h.s.cfgMu.RLock()
	defer h.s.cfgMu.RUnlock()
	sched := h.s.cfg.Schedule
	if h.s.cfg.FindPairedToken(monloader.App) == nil {
		sched.LookupPTR, sched.LookupBooru = false, false
	}
	names := make([]string, len(h.s.cfg.Galleries))
	for i, g := range h.s.cfg.Galleries {
		names[i] = g.Name
	}
	return sched, names
}

func (h schedHost) Config() *config.Config { return h.s.cfgSnapshot() }

func (h schedHost) Naming(name string) gallery.Naming { return h.s.ingestNaming(name) }

func (h schedHost) MemoryMark() func(scope string) { return autotagMemoryMark() }
