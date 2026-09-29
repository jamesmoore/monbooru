package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
)

func (h *Handler) serveImageFile(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	canonPath, fileType, ok := containedCanonical(w, g, id)
	if !ok {
		return
	}
	if ct := gallery.MIMEForFileType(fileType); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Disposition", gallery.ContentDispositionFor(canonPath))
	http.ServeFile(w, r, canonPath)
}

func containedCanonical(w http.ResponseWriter, g Gallery, id int64) (canonPath, fileType string, ok bool) {
	if err := g.DB.Read.QueryRow(
		`SELECT canonical_path, file_type FROM images WHERE id = ?`, id,
	).Scan(&canonPath, &fileType); err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return "", "", false
	}
	if !gallery.NamedInside(g.GalleryPath, canonPath) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return "", "", false
	}
	return canonPath, fileType, true
}

func (h *Handler) serveThumbnail(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndExistingID(w, r)
	if !ok {
		return
	}
	thumbPath := filepath.Join(g.ThumbnailsPath, strconv.FormatInt(id, 10)+".jpg")
	if _, err := os.Stat(thumbPath); err != nil {
		apiError(w, http.StatusNotFound, "not_found", "thumbnail not found")
		return
	}
	http.ServeFile(w, r, thumbPath)
}

func (h *Handler) serveMangaPage(w http.ResponseWriter, r *http.Request) {
	h.serveMangaPagePath(w, r, gallery.EnsureMangaPage)
}

func (h *Handler) serveMangaPageThumb(w http.ResponseWriter, r *http.Request) {
	h.serveMangaPagePath(w, r, gallery.EnsureMangaPageThumb)
}

func (h *Handler) serveMangaPagePath(
	w http.ResponseWriter, r *http.Request,
	ensure func(thumbnailsPath, canonPath string, imageID int64, n int) (string, error),
) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid page number")
		return
	}
	canonPath, fileType, ok := containedCanonical(w, g, id)
	if !ok {
		return
	}
	if fileType != models.FileTypeCBZ {
		apiError(w, http.StatusNotFound, "not_found", "image is not a manga archive")
		return
	}
	page, err := ensure(g.ThumbnailsPath, canonPath, id, n)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "page not found")
		return
	}
	http.ServeFile(w, r, page)
}
