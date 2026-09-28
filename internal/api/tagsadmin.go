package api

import (
	"cmp"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// t must carry its category name and color, which GetTag fills and
// GetOrCreateTag does not.
func toTagResponse(t *models.Tag) tagResponse {
	resp := tagResponse{
		ID:         t.ID,
		Name:       t.Name,
		Category:   t.CategoryName,
		Color:      t.CategoryColor,
		UsageCount: t.UsageCount,
		IsAlias:    t.IsAlias,
		Origin:     t.Origin,
	}
	if !t.LastUsedAt.IsZero() {
		resp.LastUsedAt = t.LastUsedAt.UTC().Format(time.RFC3339)
	}
	return resp
}

func resolveCategoryID(g Gallery, name string) (int64, bool, error) {
	name = strings.TrimSpace(name)
	name = cmp.Or(name, "general")
	return tags.CategoryIDByName(g.DB, name)
}

type sentinelStatus struct {
	err    error
	status int
	code   string
}

func writeSentinelError(w http.ResponseWriter, err error, table []sentinelStatus) bool {
	for _, e := range table {
		if errors.Is(err, e.err) {
			apiError(w, e.status, e.code, err.Error())
			return true
		}
	}
	return false
}

// The rename, alias and merge errors the service has no sentinel for
// match by phrase.
func writeTagError(w http.ResponseWriter, err error) {
	if writeSentinelError(w, err, []sentinelStatus{
		{tags.ErrTagNotFound, http.StatusNotFound, "not_found"},
		{tags.ErrCategoryNotFound, http.StatusBadRequest, "invalid_request"},
		{tags.ErrAliasNameInUse, http.StatusConflict, "conflict"},
		{tags.ErrImplicationCycle, http.StatusConflict, "conflict"},
		{tags.ErrInvalidTagName, http.StatusBadRequest, "invalid_request"},
		{tags.ErrNonCanonicalRating, http.StatusBadRequest, "invalid_request"},
		{tags.ErrRatingTagImmutable, http.StatusBadRequest, "invalid_request"},
		{tags.ErrRatingCategoryClosed, http.StatusBadRequest, "invalid_request"},
	}) {
		return
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "already exists"):
		apiError(w, http.StatusConflict, "conflict", msg)
	case strings.Contains(msg, "not found"):
		apiError(w, http.StatusNotFound, "not_found", msg)
	case strings.Contains(msg, "itself"), strings.Contains(msg, "alias"):
		apiError(w, http.StatusBadRequest, "invalid_request", msg)
	default:
		apiError(w, http.StatusInternalServerError, "internal_error", msg)
	}
}

func (h *Handler) createTag(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body struct {
		Name     string `json:"name"`
		Category string `json:"category"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	catID, found, err := resolveCategoryID(g, body.Category)
	if serverError(w, err) {
		return
	}
	if !found {
		apiError(w, http.StatusBadRequest, "invalid_request", "unknown category: "+body.Category)
		return
	}
	tag, err := g.TagSvc.GetOrCreateTagFrom(body.Name, catID, "api")
	if err != nil {
		writeTagError(w, err)
		return
	}
	full, err := g.TagSvc.GetTag(tag.ID)
	if serverError(w, err) {
		return
	}
	g.invalidate()
	WriteJSON(w, http.StatusCreated, toTagResponse(full))
}

func (h *Handler) patchTag(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var body struct {
		Name     *string `json:"name"`
		Category *string `json:"category"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == nil && body.Category == nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "name or category is required")
		return
	}
	var catID int64
	if body.Category != nil {
		cid, found, cerr := resolveCategoryID(g, *body.Category)
		if serverError(w, cerr) {
			return
		}
		if !found {
			apiError(w, http.StatusBadRequest, "invalid_request", "unknown category: "+*body.Category)
			return
		}
		catID = cid
	}
	if body.Name != nil {
		if err := g.TagSvc.RenameTag(id, *body.Name); err != nil {
			writeTagError(w, err)
			return
		}
	}
	if body.Category != nil {
		if err := g.TagSvc.ChangeTagCategory(id, catID); err != nil {
			writeTagError(w, err)
			return
		}
	}
	full, err := g.TagSvc.GetTag(id)
	if err != nil {
		writeTagError(w, err)
		return
	}
	g.invalidate()
	WriteJSON(w, http.StatusOK, toTagResponse(full))
}

func (h *Handler) deleteTag(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if err := g.TagSvc.DeleteTag(id); err != nil {
		writeTagError(w, err)
		return
	}
	g.invalidate()
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) createAlias(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body struct {
		Name        string `json:"name"`
		Category    string `json:"category"`
		CanonicalID int64  `json:"canonical_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	if body.CanonicalID == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "canonical_id is required")
		return
	}
	catID, found, err := resolveCategoryID(g, body.Category)
	if serverError(w, err) {
		return
	}
	if !found {
		apiError(w, http.StatusBadRequest, "invalid_request", "unknown category: "+body.Category)
		return
	}
	alias, err := g.TagSvc.CreateAliasFrom(body.Name, catID, body.CanonicalID, "api")
	if err != nil {
		writeTagError(w, err)
		return
	}
	g.invalidate()
	WriteJSON(w, http.StatusCreated, toTagResponse(alias))
}

func (h *Handler) mergeTags(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body struct {
		AliasID     int64 `json:"alias_id"`
		CanonicalID int64 `json:"canonical_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.AliasID == 0 || body.CanonicalID == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "alias_id and canonical_id are required")
		return
	}
	if err := g.TagSvc.MergeTags(body.AliasID, body.CanonicalID); err != nil {
		writeTagError(w, err)
		return
	}
	canon, err := g.TagSvc.GetTag(body.CanonicalID)
	if err != nil {
		writeTagError(w, err)
		return
	}
	g.invalidate()
	WriteJSON(w, http.StatusOK, toTagResponse(canon))
}

type implicationJSON struct {
	ParentID        int64  `json:"parent_id"`
	ImpliedID       int64  `json:"implied_id"`
	ImpliedName     string `json:"implied_name"`
	ImpliedCategory string `json:"implied_category"`
}

func (h *Handler) listImplications(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if _, err := g.TagSvc.GetTag(id); err != nil {
		writeTagError(w, err)
		return
	}
	imps, err := g.TagSvc.ListImplications(id)
	if serverError(w, err) {
		return
	}
	out := make([]implicationJSON, 0, len(imps))
	for _, im := range imps {
		out = append(out, implicationJSON{
			ParentID:        im.ParentID,
			ImpliedID:       im.ImpliedID,
			ImpliedName:     im.ImpliedName,
			ImpliedCategory: im.ImpliedCategoryName,
		})
	}
	WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) addImplication(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	parentID, ok := apiPathInt64(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		ImpliedID int64 `json:"implied_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ImpliedID == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "implied_id is required")
		return
	}
	isNew, err := g.TagSvc.AddImplicationFrom(parentID, body.ImpliedID, "api")
	if err != nil {
		writeTagError(w, err)
		return
	}
	if isNew {
		w.WriteHeader(http.StatusCreated)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) removeImplication(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	parentID, ok := apiPathInt64(w, r, "id")
	if !ok {
		return
	}
	impliedID, ok := apiPathInt64(w, r, "impliedID")
	if !ok {
		return
	}
	if err := g.TagSvc.RemoveImplication(parentID, impliedID); err != nil {
		writeTagError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
