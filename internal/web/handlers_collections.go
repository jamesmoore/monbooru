package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
)

const collectionsPerPage = 60

// Generous on purpose: the template clips the strip to one line.
const collectionPreviewSamples = 16

const collectionOrderWindow = 200

type collectionsPageData struct {
	baseData
	Collections []gallery.CollectionSummary
	Total       int
	Page        int
	TotalPages  int
	Prefix      string
	Sort        string
}

func (s *Server) collectionsHandler(w http.ResponseWriter, r *http.Request) {
	// A rename or dissolve job changes the listing a reload must show.
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	prefix := strings.TrimSpace(q.Get("q"))
	sortStr := q.Get("sort")
	if sortStr != "name" {
		sortStr = "size"
	}
	page := 1
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		page = p
	}
	excludeIDs := resolveCeiling(r, s.active()).ExcludedTagIDs()

	total, err := gallery.CountCollections(s.db(), prefix, excludeIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	totalPages := (total + collectionsPerPage - 1) / collectionsPerPage
	if total > 0 && page > totalPages {
		page = totalPages
	}

	list, err := gallery.ListCollections(s.db(), prefix, sortStr, collectionsPerPage, (page-1)*collectionsPerPage, excludeIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := make([]string, len(list))
	for i := range list {
		names[i] = list[i].Name
	}
	samples, err := gallery.CollectionSamples(s.db(), names, collectionPreviewSamples, excludeIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range list {
		list[i].Samples = samples[strings.ToLower(list[i].Name)]
	}

	s.renderTemplate(w, "collections.html", collectionsPageData{
		baseData:    s.base(r, "collections", "Collections - "+s.booruName()),
		Collections: list,
		Total:       total,
		Page:        page,
		TotalPages:  totalPages,
		Prefix:      prefix,
		Sort:        sortStr,
	})
}

// Renaming onto an existing label merges the two.
func (s *Server) renameCollectionPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	oldName := strings.TrimSpace(r.FormValue("prev"))
	newName := strings.TrimSpace(r.FormValue("name"))
	if oldName == "" || newName == "" {
		flashStatus(w, http.StatusBadRequest, "Both the current and the new collection name are required.")
		return
	}
	if utf8.RuneCountInString(newName) > maxExternalSourceLen {
		flashStatus(w, http.StatusBadRequest, "Collection label too long.")
		return
	}
	if oldName == newName {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s.startCollectionJob(w, oldName, func(ids []int64) {
		s.runRenameCollection(ids, oldName, newName)
	})
}

func (s *Server) collectionFindRelationsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name, ok := requiredFormFlash(w, r, "collection", "Collection label required.")
	if !ok {
		return
	}
	enabled := r.FormValue("enabled") == "1"
	if err := gallery.SetCollectionFindRelations(s.db(), name, enabled); err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not update the collection.")
		return
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	setFlashHeader(w, "Find relations "+verb+" for "+name+".", "ok", nil)
	s.renderTemplate(w, "partials/collection_find_relations.html", map[string]any{
		"Name":          name,
		"FindRelations": enabled,
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
	})
}

func (s *Server) collectionOrderDialog(w http.ResponseWriter, r *http.Request) {
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/collections", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		http.Error(w, "collection required", http.StatusBadRequest)
		return
	}
	limit := collectionOrderWindow
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	excludeIDs := resolveCeiling(r, s.active()).ExcludedTagIDs()
	members, err := gallery.CollectionMembers(s.db(), name, excludeIDs, limit+1, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hasMore := len(members) > limit
	if hasMore {
		members = members[:limit]
	}
	ceilingHidden, _ := gallery.CollectionCeilingHidden(s.db(), name, excludeIDs)
	s.renderTemplate(w, "partials/collection_order.html", map[string]any{
		"Name":          name,
		"Members":       members,
		"Gallery":       s.activeGallery(),
		"HasMore":       hasMore,
		"NextLimit":     limit + collectionOrderWindow,
		"CeilingHidden": ceilingHidden,
	})
}

func (s *Server) reorderCollectionPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name, ok := requiredFormFlash(w, r, "collection", "Collection label required.")
	if !ok {
		return
	}
	if r.FormValue("mode") == "filename" {
		if err := gallery.SortCollectionByFilename(s.db(), name); err != nil {
			flashStatus(w, http.StatusInternalServerError, "Could not reorder the collection.")
			return
		}
		s.active().InvalidateCaches()
		writeInlineFlash(w, "ok", "Ordered by filename.")
		return
	}
	raw, ok := requiredFormFlash(w, r, "ids", "Click at least one image first.")
	if !ok {
		return
	}
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			flashStatus(w, http.StatusBadRequest, "Bad image id list.")
			return
		}
		ids = append(ids, id)
	}
	clicked := len(ids)
	// The dialog shows only what the ceiling lets through, so hidden members
	// go after the clicked ones rather than losing their position.
	hidden, err := gallery.CollectionHiddenOrderedIDs(s.db(), name, resolveCeiling(r, s.active()).ExcludedTagIDs())
	if err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not reorder the collection.")
		return
	}
	ids = append(ids, hidden...)
	if err := gallery.ReorderCollection(s.db(), name, ids); err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not reorder the collection.")
		return
	}
	s.active().InvalidateCaches()
	writeInlineFlash(w, "ok", fmt.Sprintf("Ordered %d image(s).", clicked))
}

func (s *Server) startCollectionJob(w http.ResponseWriter, name string, run func([]int64)) {
	ids, err := gallery.CollectionMemberIDs(s.db(), name)
	if err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not read the collection.")
		return
	}
	if len(ids) == 0 {
		flashStatus(w, http.StatusBadRequest, "That collection no longer exists.")
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	go run(ids)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) dissolveCollectionPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name, ok := requiredFormFlash(w, r, "collection", "Collection label required.")
	if !ok {
		return
	}
	s.startCollectionJob(w, name, func(ids []int64) {
		s.runBatchCollection(ids, name, "remove")
		// So a later collection reusing the label starts opted out.
		if _, err := s.db().Write.Exec(
			`DELETE FROM collection_find_relations WHERE name = ?`, name); err != nil {
			logx.Debugf("dissolve collection find-relations flag: %v", err)
		}
	})
}

func (s *Server) runRenameCollection(ids []int64, oldName, newName string) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)
	// A case-only rename hits the same NOCASE label: merging would delete
	// the very rows it is meant to recase.
	merging := !strings.EqualFold(oldName, newName)

	// Read once before any chunk: the target's own members are never
	// renumbered, so the offset holds for the whole job.
	var posOffset int
	if merging {
		if err := s.db().Read.QueryRow(
			`SELECT COALESCE(MAX(position), 0) FROM image_collections WHERE name = ?`, newName,
		).Scan(&posOffset); err != nil {
			logx.Debugf("rename collection position offset: %v", err)
		}
	}

	processed, cancelled, err := jobs.Chunked(ctx, s.jobs, ids, chunkSize, "renaming collection", func(chunk []int64) error {
		return gallery.RenameCollectionForImages(s.db(), chunk, oldName, newName, posOffset, merging)
	})
	if err == nil {
		// On a merge the target's own flag wins; the DELETE is merge-only,
		// since on a case-only rename it would match the recased row.
		if _, e := s.db().Write.Exec(
			`UPDATE OR IGNORE collection_find_relations SET name = ? WHERE name = ?`, newName, oldName); e != nil {
			logx.Debugf("rename collection find-relations flag: %v", e)
		} else if merging {
			if _, e := s.db().Write.Exec(
				`DELETE FROM collection_find_relations WHERE name = ?`, oldName); e != nil {
				logx.Debugf("rename collection find-relations flag: %v", e)
			}
		}
		s.active().InvalidateCaches()
	}
	s.finishJob(err, cancelled, fmt.Sprintf("rename cancelled (%d/%d processed)", processed, total), fmt.Sprintf("Renamed collection across %d image(s).", processed))
}
