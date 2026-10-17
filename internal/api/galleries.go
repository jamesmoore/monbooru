package api

import (
	"net/http"
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
		images, imagesOK := g.VisibleCount()
		tags, tagsOK := g.TagCount()
		if !imagesOK || !tagsOK {
			apiError(w, http.StatusInternalServerError, "internal_error", "could not count gallery "+gc.Name)
			return
		}
		out = append(out, galleryListEntry{Name: gc.Name, Images: images, Tags: tags, Active: gc.Name == activeName})
	}
	WriteJSON(w, http.StatusOK, out)
}
