package web

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tags"
	"github.com/monbooru/monbooru/internal/upgrade"
)

func (s *Server) ratingCeilingPost(w http.ResponseWriter, r *http.Request) {
	level := r.URL.Query().Get("level")
	level = cmp.Or(level, r.FormValue("level"))
	writeRatingCookie(w, level)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) viewPrefsPost(w http.ResponseWriter, r *http.Request) {
	if v := r.FormValue("page_size"); v != "" {
		age := 31_536_000
		if v == "default" {
			v, age = "", -1
		} else if n, err := strconv.Atoi(v); err != nil || !slices.Contains(PageSizeOptions, n) {
			http.Error(w, "unknown page size", http.StatusBadRequest)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     pageSizeCookieName,
			Value:    v,
			Path:     "/",
			HttpOnly: true,
			MaxAge:   age,
			SameSite: http.SameSiteLaxMode,
		})
	}
	if v := r.FormValue("thumb"); v != "" {
		if v != "s" && v != "m" && v != "l" {
			http.Error(w, "unknown thumbnail size", http.StatusBadRequest)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     thumbSizeCookieName,
			Value:    v,
			Path:     "/",
			HttpOnly: true,
			MaxAge:   31_536_000,
			SameSite: http.SameSiteLaxMode,
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) toggleBoolColumn(w http.ResponseWriter, r *http.Request, column, onHTML, offHTML string, oob func(*http.Request) string) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	var newVal int
	if err := s.db().Write.QueryRow(
		`UPDATE images SET `+column+` = 1 - `+column+` WHERE id = ? RETURNING `+column, id,
	).Scan(&newVal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if cx := s.active(); cx != nil {
		cx.InvalidateCaches()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if newVal == 1 {
		_, _ = w.Write([]byte(onHTML))
	} else {
		_, _ = w.Write([]byte(offHTML))
	}
	if oob != nil {
		_, _ = w.Write([]byte(oob(r)))
	}
}

// FE0E pins the heart to text style; bare U+2665 turns into an oversized emoji.
func (s *Server) toggleFavorite(w http.ResponseWriter, r *http.Request) {
	s.toggleBoolColumn(w, r, "is_favorited",
		`<button type="submit" id="fav-btn" class="btn-fav active" title="Unfavorite">♥&#xFE0E;</button>`,
		`<button type="submit" id="fav-btn" class="btn-fav" title="Favorite">♡</button>`,
		nil,
	)
}

func (s *Server) toggleInbox(w http.ResponseWriter, r *http.Request) {
	s.toggleBoolColumn(w, r, "is_inbox",
		`<button type="submit" id="inbox-btn" class="btn-inbox active" title="Archive (i)">In inbox</button>`,
		`<button type="submit" id="inbox-btn" class="btn-inbox" title="Send to inbox (i)">Archived</button>`,
		s.inboxNavOOB,
	)
}

// Only the count span is swapped: the link's classes depend on a search
// this POST doesn't carry.
func (s *Server) inboxNavOOB(r *http.Request) string {
	cx := s.active()
	if cx == nil {
		return ""
	}
	n, err := cx.InboxCountUnder(resolveCeiling(r, cx))
	if err != nil {
		return ""
	}
	suffix := ""
	if n > 0 {
		suffix = fmt.Sprintf(" (%d)", n)
	}
	return fmt.Sprintf(`<span id="inbox-nav-count" hx-swap-oob="true">%s</span>`, suffix)
}

func (s *Server) deleteImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}

	back := parseBackContext(r)

	// With a ref, back_* describes the source's search, not this image's.
	var refID *int64
	if refStr := r.URL.Query().Get("ref"); refStr != "" {
		if parsed, err := strconv.ParseInt(refStr, 10, 64); err == nil && parsed != id {
			refID = &parsed
		}
	}

	// Before the delete: the neighbours can't be found once this row is gone.
	var prevID, nextID *int64
	if refID == nil && (back.Sort != "" || back.Q != "") {
		sortStr := back.Sort
		sortStr = cmp.Or(sortStr, "newest")
		orderStr := back.Order
		orderStr = cmp.Or(orderStr, search.DefaultOrder(sortStr))
		prevID, nextID = s.findAdjacentImages(r.Context(), id, back.Q, sortStr, orderStr, back.Seed, resolveCeiling(r, s.active()))
	}

	_, err := gallery.DeleteImage(s.db(), s.boundary(), s.thumbnailsPath(), id, tags.RemoveAllTagsFromImageTx, s.onImageDeleteCallback())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		logx.Errorf("delete image %d: %v", id, err)
		flashStatus(w, http.StatusInternalServerError, "Delete failed; check server log.")
		return
	}
	s.active().InvalidateCaches()

	redirectURL := ""
	switch {
	case refID != nil:
		redirectURL = back.DetailURL(*refID)
	case nextID != nil:
		redirectURL = back.DetailURL(*nextID)
	case prevID != nil:
		redirectURL = back.DetailURL(*prevID)
	default:
		redirectURL = back.GalleryURL()
	}

	flashText := fmt.Sprintf("Deleted image #%d.", id)
	if isHTMXRequest(r) {
		// A redirect would push a history entry that drops the ref chain; the
		// client goes back instead, the fallback serving a tab with no history.
		if refID != nil {
			setFlashHeader(w, flashText, "ok", map[string]any{
				"delete-go-back": map[string]any{"fallback": redirectURL},
			})
			w.WriteHeader(http.StatusOK)
			return
		}
		setFlashHeader(w, flashText, "ok", nil)
		w.Header().Set("HX-Redirect", redirectURL)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

func (s *Server) promoteCanonical(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string, code int) {
		if isHTMXRequest(r) {
			setFlashHeader(w, msg, "err", nil)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, msg, code)
	}
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	newCanonical := r.FormValue("path")
	if newCanonical == "" {
		fail("path required", http.StatusBadRequest)
		return
	}

	// Tracked aliases only: any other path lets serveImageFile serve that file.
	var aliasExists int
	if err := s.db().Read.QueryRow(
		`SELECT COUNT(*) FROM image_paths WHERE image_id = ? AND path = ?`,
		id, newCanonical,
	).Scan(&aliasExists); err != nil {
		fail(err.Error(), http.StatusInternalServerError)
		return
	}
	if aliasExists == 0 {
		fail("path is not an alias of this image", http.StatusBadRequest)
		return
	}
	if _, statErr := os.Stat(newCanonical); statErr != nil {
		fail("cannot set canonical: file is missing on disk", http.StatusBadRequest)
		return
	}

	if err := gallery.PromoteCanonicalByPath(s.db(), s.boundary(), id, newCanonical); err != nil {
		code := http.StatusInternalServerError
		if errors.As(err, new(gallery.Exclusion)) {
			code = http.StatusBadRequest
		}
		fail(err.Error(), code)
		return
	}
	s.active().InvalidateCaches()
	hxDone(w, r, "Canonical path updated.", "", fmt.Sprintf("/images/%d", id))
}

const (
	maxExternalSourceLen  = gallery.MaxSourceLabelLen
	maxExternalURLLen     = gallery.MaxSourceURLLen
	maxImageCommentaryLen = gallery.MaxCommentaryLen
	maxImageOriginalLen   = gallery.MaxOriginalLen
	maxAnnotationBodyLen  = gallery.MaxAnnotationBodyLen
	maxImageNoteLen       = 10000
)

func (s *Server) setSource(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	site := strings.TrimSpace(r.FormValue("site"))
	url := strings.TrimSpace(r.FormValue("url"))
	if site == "" && url == "" {
		externalErr(w, r, "source label or url required", http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(site) > maxExternalSourceLen {
		externalErr(w, r, fmt.Sprintf("source too long (max %d chars)", maxExternalSourceLen), http.StatusBadRequest)
		return
	}
	if url != "" {
		if utf8.RuneCountInString(url) > maxExternalURLLen {
			externalErr(w, r, fmt.Sprintf("url too long (max %d chars)", maxExternalURLLen), http.StatusBadRequest)
			return
		}
		if !gallery.ValidExternalURL(url) {
			externalErr(w, r, "url must start with http:// or https://", http.StatusBadRequest)
			return
		}
	}
	postID := strings.TrimSpace(r.FormValue("post_id"))
	prevSite := strings.TrimSpace(r.FormValue("prev_site"))
	prevPost := strings.TrimSpace(r.FormValue("prev_post"))
	// A url-only origin is keyed ("", ""); has_prev tells relabel from add.
	hasPrev := r.FormValue("has_prev") == "1" || prevSite != "" || prevPost != ""
	if !s.imageExists(id) {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	if hasPrev && (!strings.EqualFold(prevSite, site) || prevPost != postID) {
		if err := gallery.RenameSourceMembership(s.db(), id, prevSite, prevPost, site, postID, url); err != nil {
			externalErr(w, r, err.Error(), http.StatusInternalServerError)
			return
		}
	} else if err := gallery.AddSourceMembership(s.db(), id, site, postID, url); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.imageEditDone(w, r, id, "Source updated.")
}

func (s *Server) sourceMembershipAction(w http.ResponseWriter, r *http.Request, successMsg string, action func(id int64, site, postID string) error) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	site := strings.TrimSpace(r.FormValue("site"))
	postID := strings.TrimSpace(r.FormValue("post_id"))
	s.applyImageEdit(w, r, id, successMsg, func() error { return action(id, site, postID) })
}

func (s *Server) removeSource(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Source removed.", func(id int64, site, postID string) error {
		return gallery.RemoveSourceMembership(s.db(), id, site, postID)
	})
}

func (s *Server) makeSourcePrimary(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Primary source updated.", func(id int64, site, postID string) error {
		return gallery.MakeSourcePrimary(s.db(), id, site, postID)
	})
}

func (s *Server) keepLocalFile(w http.ResponseWriter, r *http.Request) {
	kept := r.FormValue("keep") == "1"
	msg := "Upgrade offered again."
	if kept {
		msg = "Keeping the local file."
	}
	s.sourceMembershipAction(w, r, msg, func(id int64, site, postID string) error {
		return gallery.SetSourceUpgradeKept(s.db(), id, site, postID, kept)
	})
}

func (s *Server) fetchSource(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	url, ok := requiredFormExternal(w, r, "url", "this source has no url to fetch")
	if !ok {
		return
	}
	galleryName := s.activeGallery()
	if pageGalleryStale(w, r, galleryName) {
		return
	}
	s.fetchStatus.record(galleryName, id, "pending", "")
	if err := s.enqueueMetadataFetch(r.Context(), id, galleryName, url); err != nil {
		s.fetchStatus.clear(galleryName, id)
		externalErr(w, r, "could not reach monloader: "+err.Error(), http.StatusBadGateway)
		return
	}
	respondFetchPending(w, r, id)
}

func (s *Server) lookupImage(w http.ResponseWriter, r *http.Request) {
	id, cx, galleryName, ok := s.imageAndGallery(w, r)
	if !ok {
		return
	}
	backend := r.FormValue("backend")
	if backend != "all" && backend != "ptr" && backend != "booru" {
		externalErr(w, r, "unknown lookup backend", http.StatusBadRequest)
		return
	}
	var sha, storedMD5 string
	if err := cx.DB.Read.QueryRow(
		`SELECT sha256, md5 FROM images WHERE id = ?`, id,
	).Scan(&sha, &storedMD5); err != nil {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	var md5 string
	if backend != "ptr" {
		var err error
		if md5, err = lookupMD5(r.Context(), cx, id, storedMD5); err != nil {
			externalErr(w, r, "cannot hash the file: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	hashes := "md5 " + md5 + ", sha256 " + sha
	switch backend {
	case "ptr":
		hashes = "sha256 " + sha
	case "booru":
		hashes = "md5 " + md5
	}
	s.fetchStatus.recordLookup(galleryName, id, hashes)
	jobID, err := s.enqueueHashLookup(r.Context(), id, galleryName, backend, md5, sha, false, false)
	if err != nil {
		s.fetchStatus.clear(galleryName, id)
		if errors.Is(err, errPTRUnavailable) {
			externalErr(w, r, err.Error(), http.StatusConflict)
			return
		}
		externalErr(w, r, "could not reach monloader: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.recordLookupEnqueued(cx, id, backend, jobID)
	respondFetchPending(w, r, id)
}

// Replacing bytes is the strongest fetch, so it never stacks on a pending one.
func (s *Server) replaceImage(w http.ResponseWriter, r *http.Request) {
	id, cx, galleryName, ok := s.imageAndGallery(w, r)
	if !ok {
		return
	}
	site := strings.TrimSpace(r.FormValue("site"))
	postID := strings.TrimSpace(r.FormValue("post_id"))
	src := models.ImageSource{Site: site, PostID: postID}
	if err := cx.DB.Read.QueryRow(
		`SELECT url, similarity, md5_match, upgrade_kept FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
		id, site, postID,
	).Scan(&src.URL, &src.Similarity, &src.MD5Match, &src.UpgradeKept); err != nil {
		externalErr(w, r, "source not found", http.StatusNotFound)
		return
	}
	if !upgrade.Eligible(src) {
		externalErr(w, r, "this source's file is not known to differ from the local one", http.StatusConflict)
		return
	}
	if e, ok := s.fetchStatus.load(galleryName, id); ok && e.State == "pending" {
		externalErr(w, r, "a fetch is already running for this image; wait for it to finish", http.StatusConflict)
		return
	}
	s.fetchStatus.record(galleryName, id, "pending", "")
	if err := s.enqueueReplace(r.Context(), id, galleryName, src.URL); err != nil {
		s.fetchStatus.clear(galleryName, id)
		externalErr(w, r, "could not reach monloader: "+err.Error(), http.StatusBadGateway)
		return
	}
	respondFetchPending(w, r, id)
}

// Free routes hold no ctxMu across a peer call, so they snapshot the name.
func (s *Server) imageAndGallery(w http.ResponseWriter, r *http.Request) (int64, *galleryCtx, string, bool) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return 0, nil, "", false
	}
	cx := s.active()
	if cx == nil {
		externalErr(w, r, "no active gallery", http.StatusServiceUnavailable)
		return 0, nil, "", false
	}
	if pageGalleryStale(w, r, cx.Name) {
		return 0, nil, "", false
	}
	return id, cx, cx.Name, true
}

func imageIDForm(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return 0, false
	}
	if err := r.ParseForm(); err != nil {
		externalErr(w, r, "bad form", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func externalErr(w http.ResponseWriter, r *http.Request, msg string, code int) {
	hxErr(w, r, msg, msg, code)
}

func (s *Server) imageEditDone(w http.ResponseWriter, r *http.Request, id int64, msg string) {
	s.active().InvalidateCaches()
	hxDone(w, r, msg, "", "/images/"+strconv.FormatInt(id, 10))
}

func (s *Server) applyImageEdit(w http.ResponseWriter, r *http.Request, id int64, msg string, edit func() error) {
	if !s.imageExists(id) {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	if err := edit(); err != nil {
		externalErr(w, r, err.Error(), imageEditStatus(err))
		return
	}
	s.imageEditDone(w, r, id, msg)
}

func imageEditStatus(err error) int {
	switch {
	case errors.Is(err, gallery.ErrSourceLabelRequired):
		return http.StatusBadRequest
	case errors.Is(err, gallery.ErrSourceNotFound), errors.Is(err, gallery.ErrAnnotationNotFound):
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func (s *Server) imageExists(id int64) bool {
	var one int
	return s.db().Read.QueryRow(`SELECT 1 FROM images WHERE id = ?`, id).Scan(&one) == nil
}

func hxErr(w http.ResponseWriter, r *http.Request, htmxMsg, plainMsg string, code int) {
	if isHTMXRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		flashStatus(w, http.StatusOK, htmxMsg)
		return
	}
	http.Error(w, plainMsg, code)
}

func (s *Server) setNote(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if utf8.RuneCountInString(note) > maxImageNoteLen {
		externalErr(w, r, fmt.Sprintf("note too long (max %d chars)", maxImageNoteLen), http.StatusBadRequest)
		return
	}
	res, err := s.db().Write.Exec(`UPDATE images SET note = ? WHERE id = ?`, note, id)
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	hxDone(w, r, "Note updated.", "", "/images/"+strconv.FormatInt(id, 10))
}

func (s *Server) setSourceText(w http.ResponseWriter, r *http.Request, field, label string, maxLen int, successMsg string, setter func(id int64, site, postID, val string) error) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	site, ok := requiredFormExternal(w, r, "site", "source label required")
	if !ok {
		return
	}
	val := strings.TrimSpace(r.FormValue(field))
	if utf8.RuneCountInString(val) > maxLen {
		externalErr(w, r, fmt.Sprintf("%s too long (max %d chars)", label, maxLen), http.StatusBadRequest)
		return
	}
	postID := strings.TrimSpace(r.FormValue("post_id"))
	s.applyImageEdit(w, r, id, successMsg, func() error { return setter(id, site, postID, val) })
}

func (s *Server) setSourceCommentary(w http.ResponseWriter, r *http.Request) {
	s.setSourceText(w, r, "commentary", "commentary", maxImageCommentaryLen, "Commentary updated.",
		func(id int64, site, postID, val string) error {
			return gallery.SetSourceCommentary(s.db(), id, site, postID, val)
		})
}

func (s *Server) removeSourceCommentary(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Commentary removed.", func(id int64, site, postID string) error {
		return gallery.SetSourceCommentary(s.db(), id, site, postID, "")
	})
}

func (s *Server) setSourceTranslation(w http.ResponseWriter, r *http.Request) {
	s.setSourceText(w, r, "commentary_translated", "translation", maxImageCommentaryLen, "Translation updated.",
		func(id int64, site, postID, val string) error {
			return gallery.SetSourceCommentaryTranslated(s.db(), id, site, postID, val)
		})
}

func (s *Server) removeSourceTranslation(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Translation removed.", func(id int64, site, postID string) error {
		return gallery.SetSourceCommentaryTranslated(s.db(), id, site, postID, "")
	})
}

// Freeform, no URL check: a booru declares it as lines of URLs or plain text.
func (s *Server) setSourceOriginal(w http.ResponseWriter, r *http.Request) {
	s.setSourceText(w, r, "original", "original source", maxImageOriginalLen, "Original source updated.",
		func(id int64, site, postID, val string) error {
			return gallery.SetSourceOriginal(s.db(), id, site, postID, val)
		})
}

func (s *Server) removeSourceOriginal(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Original source removed.", func(id int64, site, postID string) error {
		return gallery.SetSourceOriginal(s.db(), id, site, postID, "")
	})
}

func (s *Server) setAnnotation(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	var wImg, hImg sql.NullInt64
	if err := s.db().Read.QueryRow(`SELECT width, height FROM images WHERE id = ?`, id).Scan(&wImg, &hImg); err != nil {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	x, okX := annotationCoord(r, "x")
	y, okY := annotationCoord(r, "y")
	bw, okW := annotationCoord(r, "w")
	bh, okH := annotationCoord(r, "h")
	if !okX || !okY || !okW || !okH {
		externalErr(w, r, "coordinates must be non-negative integers", http.StatusBadRequest)
		return
	}
	if wImg.Valid && hImg.Valid && wImg.Int64 > 0 && hImg.Int64 > 0 {
		iw, ih := int(wImg.Int64), int(hImg.Int64)
		x = min(x, iw)
		y = min(y, ih)
		bw = min(bw, iw-x)
		bh = min(bh, ih-y)
	}
	if bw <= 0 || bh <= 0 {
		externalErr(w, r, "the box has no area inside the image", http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if utf8.RuneCountInString(body) > maxAnnotationBodyLen {
		externalErr(w, r, fmt.Sprintf("annotation too long (max %d chars)", maxAnnotationBodyLen), http.StatusBadRequest)
		return
	}
	done := "Annotation added."
	if raw := strings.TrimSpace(r.FormValue("id")); raw != "" {
		annID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			externalErr(w, r, "bad annotation id", http.StatusBadRequest)
			return
		}
		if err := gallery.UpdateAnnotation(s.db(), id, annID, x, y, bw, bh, body); err != nil {
			externalErr(w, r, err.Error(), imageEditStatus(err))
			return
		}
		done = "Annotation updated."
	} else if err := gallery.AddManualAnnotation(s.db(), id, x, y, bw, bh, body); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.imageEditDone(w, r, id, done)
}

func (s *Server) removeAnnotation(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	annID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if err != nil {
		externalErr(w, r, "bad annotation id", http.StatusBadRequest)
		return
	}
	s.applyImageEdit(w, r, id, "Annotation removed.", func() error { return gallery.DeleteAnnotation(s.db(), id, annID) })
}

func (s *Server) previewMarkup(w http.ResponseWriter, r *http.Request) {
	if _, ok := imageIDForm(w, r); !ok {
		return
	}
	s.renderTemplate(w, "partials/markup_preview.html", s.renderMarkup(r.FormValue("body")))
}

func annotationCoord(r *http.Request, field string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(field)))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func (s *Server) setCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	name, ok := requiredFormExternal(w, r, "collection", "collection label required")
	if !ok {
		return
	}
	if utf8.RuneCountInString(name) > maxExternalSourceLen {
		externalErr(w, r, fmt.Sprintf("collection too long (max %d chars)", maxExternalSourceLen), http.StatusBadRequest)
		return
	}
	var order *int
	if raw := strings.TrimSpace(r.FormValue("collection_order")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			externalErr(w, r, "order must be an integer or empty", http.StatusBadRequest)
			return
		}
		if n < 1 {
			externalErr(w, r, "order must be 1 or higher", http.StatusBadRequest)
			return
		}
		order = &n
	}
	var err error
	if prev := strings.TrimSpace(r.FormValue("prev")); prev != "" && !strings.EqualFold(prev, name) {
		err = gallery.RenameCollectionMembership(s.db(), id, prev, name, order)
	} else {
		err = gallery.AddCollectionMembership(s.db(), id, name, order)
	}
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.imageEditDone(w, r, id, "Collection updated.")
}

func (s *Server) removeCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	name, ok := requiredFormExternal(w, r, "collection", "collection label required")
	if !ok {
		return
	}
	s.applyImageEdit(w, r, id, "Collection removed.", func() error { return gallery.RemoveCollectionMembership(s.db(), id, name) })
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	pathID, ok := pathInt64(w, r, "pathID")
	if !ok {
		return
	}

	var isCanon int
	var aliasPath string
	var canonicalPath string
	var size int64
	if err := s.db().Read.QueryRow(
		`SELECT ip.is_canonical, ip.path, i.canonical_path, i.file_size
		 FROM image_paths ip JOIN images i ON i.id = ip.image_id
		 WHERE ip.id = ? AND ip.image_id = ?`, pathID, id,
	).Scan(&isCanon, &aliasPath, &canonicalPath, &size); err != nil {
		http.Error(w, "alias path not found", http.StatusNotFound)
		return
	}
	if isCanon == 1 {
		http.Error(w, "cannot delete canonical path", http.StatusBadRequest)
		return
	}

	if err := gallery.DeleteAliasPath(s.db(), pathID); err != nil {
		logx.Warnf("delete alias row %d: %v", pathID, err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	if aliasPath != "" {
		if err := unlinkAliasFile(s.boundary(), aliasPath, canonicalPath, size); err != nil {
			logx.Warnf("delete alias file %q: %v", aliasPath, err)
		}
	}

	if isHTMXRequest(r) {
		// Empty body for HTMX outerHTML swap - removes the row.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(""))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", id), http.StatusSeeOther)
}

// Refuses a path outside the root or the boundary, an alias that is the
// canonical file by another name (a re-entering link left such rows
// behind), and one no longer the row's size: a file written over it since.
func unlinkAliasFile(b *gallery.Boundary, aliasPath, canonicalPath string, size int64) error {
	galleryAbs, err := filepath.Abs(b.Root())
	if err != nil {
		return fmt.Errorf("resolve gallery root: %w", err)
	}
	aliasAbs, err := filepath.Abs(aliasPath)
	if err != nil {
		return fmt.Errorf("resolve alias: %w", err)
	}
	if !gallery.PathInside(galleryAbs, aliasAbs) {
		return fmt.Errorf("refuse: path %q is outside gallery root", aliasPath)
	}
	if err := b.Check(aliasAbs); err != nil {
		return fmt.Errorf("refuse: %w", err)
	}
	if isCanonicalEntry(aliasPath, canonicalPath) {
		return fmt.Errorf("refuse: path %q reaches the canonical file through a link", aliasPath)
	}
	if info, err := os.Stat(aliasPath); err == nil && info.Size() != size {
		return fmt.Errorf("refuse: %q holds %d bytes, not the row's %d", aliasPath, info.Size(), size)
	}
	if err := os.Remove(aliasPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Compares resolved names, not inodes: removing a symlink or a hard link
// never takes the canonical file.
func isCanonicalEntry(aliasPath, canonicalPath string) bool {
	if canonicalPath == "" {
		return false
	}
	if info, err := os.Lstat(aliasPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	alias, err := filepath.EvalSymlinks(aliasPath)
	if err != nil {
		return false
	}
	canonical, err := filepath.EvalSymlinks(canonicalPath)
	if err != nil {
		return false
	}
	return alias == canonical
}

// A move job even for one image: the watcher suppression hangs off it.
func (s *Server) singleImageMoveJob(w http.ResponseWriter, r *http.Request, op func(id int64) (summary string, answered bool, err error)) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	if !s.startJob(w, models.JobTypeMove) {
		return
	}
	summary, answered, err := op(id)
	if err != nil {
		s.jobs.Fail(err.Error())
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	s.active().InvalidateCaches()
	s.jobs.Complete(summary)
	if answered {
		return
	}
	if isHTMXRequest(r) {
		setFlashHeader(w, summary, "ok", nil)
		w.Header().Set("HX-Redirect", fmt.Sprintf("/images/%d", id))
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", id), http.StatusSeeOther)
}

// No folder field keeps the folder; an empty one means the gallery root.
func (s *Server) placeImage(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	rawFolder, wantFolder := strings.TrimSpace(r.FormValue("folder")), r.Form.Has("folder")
	rawName := strings.TrimSpace(r.FormValue("name"))
	inline := r.FormValue("inline") == "1"
	folderTmpl, folderErr := gallery.ParseNameTemplate(rawFolder, gallery.ScopeMove)
	nameTmpl, nameErr := gallery.ParseNameTemplate(rawName, gallery.ScopeRename)

	s.singleImageMoveJob(w, r, func(id int64) (string, bool, error) {
		if folderErr != nil {
			return "", false, folderErr
		}
		if nameErr != nil {
			return "", false, nameErr
		}
		var folder, name *string
		if wantFolder {
			rendered, err := s.singleName(r.Context(), folderTmpl, rawFolder, id)
			if err != nil {
				return "", false, err
			}
			folder = &rendered
		}
		if nameTmpl != nil {
			rendered, err := s.singleName(r.Context(), nameTmpl, rawName, id)
			if err != nil {
				return "", false, err
			}
			name = &rendered
		}
		res, err := gallery.PlaceImage(s.db(), s.boundary(), id, folder, name)
		if err != nil {
			return "", false, err
		}
		newBase := filepath.Base(res.NewCanonicalPath)
		// From the result: a plain move still submits the row's name.
		_, _, past := placeVerbs(res.Moved, res.Renamed)
		summary := fmt.Sprintf("%s image to %s.", titleCase(past), namePath(res.NewFolderPath, newBase))
		if inline {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(newBase))
			return summary, true, nil
		}
		return summary, false, nil
	})
}

func placeVerbs(folder, name bool) (verb, gerund, past string) {
	switch {
	case folder && name:
		return "file", "filing", "moved and renamed"
	case name:
		return "rename", "renaming", "renamed"
	default:
		return "move", "moving", "moved"
	}
}

func titleCase(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

func (s *Server) singleName(ctx context.Context, tmpl *gallery.NameTemplate, literal string, id int64) (string, error) {
	if !tmpl.HasTokens() {
		return literal, nil
	}
	facts, err := gallery.LoadNameFacts(ctx, s.db(), s.activeGallery(), id, 0, tmpl)
	if err != nil {
		return "", err
	}
	return tmpl.Render(facts)
}

// Exclusive upper bound of prefix's BINARY range, for a >=, < scan in
// place of LIKE 'prefix%'.
func nextPrefix(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++
			return string(b[:i+1])
		}
	}
	return prefix + "\xff"
}

// Lowercased first: NOCASE folds to lower case, so a bound built from "Z"
// would exclude every lower-case continuation.
func nocasePrefixRange(prefix string) (lo, hi string) {
	lo = strings.ToLower(prefix)
	return lo, nextPrefix(lo)
}
