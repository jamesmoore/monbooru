package web

import (
	"net/http"

	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/tags"
)

const ratingCeilingCookieName = "monbooru_rating_ceiling"

// A copy the gallery leaves out is another gallery's file, and only Go can
// tell, so every row is scanned; total counts the kept ones and only the
// window [offset, offset+limit) is held.
func duplicatePaths(r *http.Request, cx *library.Gallery, offset, limit int) (window []sha256DuplicateRow, total int, err error) {
	query := `SELECT i.id, i.canonical_path, ip.id, ip.path FROM images i
		JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 0`
	args := []any{}
	if where, wargs := resolveCeiling(r, cx).WhereOne("i.id"); where != "" {
		query += ` WHERE ` + where
		args = append(args, wargs...)
	}
	rows, err := cx.DB.Read.Query(query+` ORDER BY i.id, ip.id`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	bound := cx.Boundary()
	for rows.Next() {
		var dr sha256DuplicateRow
		if err := rows.Scan(&dr.ImageID, &dr.CanonicalPath, &dr.PathID, &dr.AliasPath); err != nil {
			return nil, 0, err
		}
		if bound.Excludes(dr.AliasPath) {
			continue
		}
		if total >= offset && total < offset+limit {
			window = append(window, dr)
		}
		total++
	}
	return window, total, rows.Err()
}

func resolveCeiling(r *http.Request, cx *library.Gallery) *Ceiling {
	return library.NewCeiling(readRatingCookie(r), cx)
}

// The value reaches the search AST: anything outside the closed set is dropped.
func readRatingCookie(r *http.Request) string {
	c, err := r.Cookie(ratingCeilingCookieName)
	if err != nil {
		return ""
	}
	if tags.IsCanonicalRating(c.Value) {
		return c.Value
	}
	return ""
}

// The top level is no ceiling at all, so it clears the cookie.
func writeRatingCookie(w http.ResponseWriter, r *http.Request, level string) {
	if rank := tags.RatingRank(level); rank >= 0 && rank < len(tags.RatingLevels)-1 {
		setPrefCookie(w, r, ratingCeilingCookieName, level)
		return
	}
	setPrefCookie(w, r, ratingCeilingCookieName, "")
}
