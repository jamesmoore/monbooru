package web

import (
	"database/sql"
	"net/http"
	"slices"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/library"
)

const ratingCeilingCookieName = "monbooru_rating_ceiling"

// Shared so the duplicate count and list agree on which rows the ceiling
// hides. A copy the gallery leaves out is another gallery's file.
func duplicatePaths(r *http.Request, cx *galleryCtx) ([]sha256DuplicateRow, error) {
	query := `SELECT i.id, i.canonical_path, ip.id, ip.path FROM images i
		JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 0`
	args := []any{}
	if where, wargs := resolveCeiling(r, cx).WhereOne("i.id"); where != "" {
		query += ` WHERE ` + where
		args = append(args, wargs...)
	}
	rows, err := db.QueryAll(cx.DB.Read, func(rows *sql.Rows) (sha256DuplicateRow, error) {
		var dr sha256DuplicateRow
		err := rows.Scan(&dr.ImageID, &dr.CanonicalPath, &dr.PathID, &dr.AliasPath)
		return dr, err
	}, query+` ORDER BY i.id, ip.id`, args...)
	if err != nil {
		return nil, err
	}
	bound := cx.Boundary()
	return slices.DeleteFunc(rows, func(dr sha256DuplicateRow) bool { return bound.Excludes(dr.AliasPath) }), nil
}

func resolveCeiling(r *http.Request, cx *galleryCtx) *Ceiling {
	return library.NewCeiling(readRatingCookie(r), cx)
}

// The value reaches the search AST: anything outside the closed set is dropped.
func readRatingCookie(r *http.Request) string {
	c, err := r.Cookie(ratingCeilingCookieName)
	if err != nil {
		return ""
	}
	switch c.Value {
	case "general", "sensitive", "questionable", "explicit":
		return c.Value
	}
	return ""
}

func writeRatingCookie(w http.ResponseWriter, level string) {
	switch level {
	case "general", "sensitive", "questionable":
		http.SetCookie(w, &http.Cookie{
			Name:     ratingCeilingCookieName,
			Value:    level,
			Path:     "/",
			HttpOnly: true,
			MaxAge:   31_536_000,
			SameSite: http.SameSiteLaxMode,
		})
	default:
		http.SetCookie(w, &http.Cookie{
			Name:   ratingCeilingCookieName,
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})
	}
}
