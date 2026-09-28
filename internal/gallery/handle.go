// Package gallery owns the files under a gallery root. Paths on disk are
// native; the folder_path column is "/"-separated on every platform.
package gallery

import (
	"sync/atomic"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tags"
)

type Handle struct {
	Name           string
	GalleryPath    string
	ThumbnailsPath string
	DBPath         string
	DB             *db.DB
	TagSvc         *tags.Service
	// A pointer, so every copy of the handle and the watcher see a redraw.
	Bounds *atomic.Pointer[Boundary]
}

func (h Handle) Boundary() *Boundary {
	if h.Bounds != nil {
		if b := h.Bounds.Load(); b != nil {
			return b
		}
	}
	return NewBoundary(h.GalleryPath, nil, nil)
}
