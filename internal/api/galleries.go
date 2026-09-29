package api

import (
	"net/http"

	"github.com/monbooru/monbooru/internal/counts"
)

type galleryListEntry struct {
	Name   string `json:"name"`
	Images int    `json:"images"`
	Tags   int    `json:"tags"`
	Active bool   `json:"active"`
}

func (h *Handler) listGalleries(w http.ResponseWriter, r *http.Request) {
	activeName := ""
	if active, ok := h.resolver(""); ok {
		activeName = active.Name
	}
	configured := h.cfg().Galleries
	out := make([]galleryListEntry, 0, len(configured))
	for _, gc := range configured {
		g, ok := h.resolver(gc.Name)
		if !ok {
			continue
		}
		entry := galleryListEntry{Name: gc.Name, Active: gc.Name == activeName}
		entry.Images, _ = counts.VisibleCount(g.DB)
		entry.Tags, _ = counts.TagCount(g.DB)
		out = append(out, entry)
	}
	WriteJSON(w, http.StatusOK, out)
}
