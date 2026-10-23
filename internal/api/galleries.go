package api

import (
	"net/http"

	"github.com/monbooru/monbooru/internal/shared/contract"
)

func (h *Handler) listGalleries(w http.ResponseWriter, r *http.Request) {
	activeName := ""
	if active, ok := h.resolver(""); ok {
		activeName = active.Name
	}
	configured := h.cfg().Galleries
	out := make([]contract.Gallery, 0, len(configured))
	for _, gc := range configured {
		g, ok := h.resolver(gc.Name)
		if !ok {
			continue
		}
		images, imagesOK := g.VisibleCount()
		tags, tagsOK := g.TagCount()
		if !imagesOK || !tagsOK {
			contract.WriteError(w, http.StatusInternalServerError, "internal_error", "could not count gallery "+gc.Name)
			return
		}
		out = append(out, contract.Gallery{Name: gc.Name, Images: images, Tags: tags, Active: gc.Name == activeName})
	}
	contract.WriteJSON(w, http.StatusOK, out)
}
