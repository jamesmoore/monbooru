package web

import (
	"net/http"

	"github.com/monbooru/monbooru/internal/library"
)

// ratingCeilingCookieName is the single point of truth for the cookie
// name. The handler at POST /internal/rating-ceiling writes it; the
// resolver below reads it; nowhere else should reference the literal.
const ratingCeilingCookieName = "monbooru_rating_ceiling"

// duplicatePathsFrom is the FROM clause both file-duplicate surfaces
// count and list against, with the rating ceiling folded in. Shared
// because the two must agree on what a duplicate file is and on hiding
// the same rows: one prints paths the other's counter would deny.
func duplicatePathsFrom(r *http.Request, cx *galleryCtx) (string, []any) {
	from := ` FROM images i
		JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 0`
	args := []any{}
	if where, wargs := resolveCeiling(r, cx).WhereOne("i.id"); where != "" {
		from += ` WHERE ` + where
		args = append(args, wargs...)
	}
	return from, args
}

// resolveCeiling reads the cookie and returns a Ceiling bound to cx, which
// may be nil when no gallery is active; library.NewCeiling says what the
// resolver still answers in that state.
func resolveCeiling(r *http.Request, cx *galleryCtx) *Ceiling {
	return library.NewCeiling(readRatingCookie(r), cx)
}

// readRatingCookie parses the cookie value. Empty string and "explicit"
// both mean "no ceiling"; anything outside the closed enum is dropped to
// "" so a stale or hand-crafted cookie can't inject arbitrary AST values.
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

// writeRatingCookie sets or clears the cookie. level=explicit (or any
// out-of-enum value) clears it so the empty-storage steady state means
// "no ceiling".
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
