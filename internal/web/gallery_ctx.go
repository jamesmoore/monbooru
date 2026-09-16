package web

import (
	"database/sql"
	"net/http"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/tags"
)

// requireActive returns the active gallery context, or writes a 503
// "no gallery" and returns false. Callers must `return` on a false
// result. Use this for any handler whose work can't proceed without a
// live DB; sub-service guards (RelationsSvc==nil, bkTree==nil) still
// belong inline because they check different fields.
func (s *Server) requireActive(w http.ResponseWriter) (*galleryCtx, bool) {
	cx := s.active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return nil, false
	}
	return cx, true
}

// Accessors below resolve to the active gallery's fields. A gallery-read
// route's RLock keeps the returned pointers stable per request.

func (s *Server) db() *db.DB {
	if cx := s.active(); cx != nil {
		return cx.DB
	}
	return nil
}

func (s *Server) tagSvc() *tags.Service {
	if cx := s.active(); cx != nil {
		return cx.TagSvc
	}
	return nil
}

// categoryExists reports whether name matches a row in tag_categories on
// the active gallery. Callers use it to disambiguate a `prefix:value`
// token that might be category-qualified or a literal tag containing a
// colon. Database errors (including nil gallery) count as "no match" so
// an ambiguous input degrades to literal.
func (s *Server) categoryExists(name string) bool {
	_, ok := s.categoryIDByName(name)
	return ok
}

// categoryIDByName resolves a category name to its row id, off the
// package that owns the table.
func (s *Server) categoryIDByName(name string) (int64, bool) {
	d := s.db()
	if d == nil {
		return 0, false
	}
	id, ok, err := tags.CategoryIDByName(d, name)
	return id, ok && err == nil
}

func (s *Server) galleryPath() string {
	if cx := s.active(); cx != nil {
		return cx.GalleryPath
	}
	return ""
}

func (s *Server) relationsSvc() *relations.Service {
	if cx := s.active(); cx != nil {
		return cx.RelationsSvc
	}
	return nil
}

// onImageDeleteCallback wires the active gallery's relations service
// into the gallery.DeleteImage signature. Returns nil when the
// service isn't available (e.g. mid-switch), so DeleteImage skips the
// relations cleanup step rather than crashing.
func (s *Server) onImageDeleteCallback() func(*sql.Tx, int64) error {
	svc := s.relationsSvc()
	if svc == nil {
		return nil
	}
	return svc.OnImageDeleteTx
}

// onImagesDeleteCallback is onImageDeleteCallback for the chunked bulk
// paths, which hand the whole chunk over so each group is decided once.
func (s *Server) onImagesDeleteCallback() func(*sql.Tx, []int64) error {
	svc := s.relationsSvc()
	if svc == nil {
		return nil
	}
	return svc.OnImagesDeleteTx
}

func (s *Server) thumbnailsPath() string {
	if cx := s.active(); cx != nil {
		return cx.ThumbnailsPath
	}
	return ""
}

func (s *Server) dbPath() string {
	if cx := s.active(); cx != nil {
		return cx.DBPath
	}
	return ""
}

// galleryCtx is the aggregate, which lives in internal/library. The alias
// keeps the local spelling every handler already uses.
type galleryCtx = library.Gallery

// Ceiling is the per-request rating ceiling, which travels with the
// gallery it filters.
type Ceiling = library.Ceiling
