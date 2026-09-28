package web

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
)

type relatedTile struct {
	ID     int64
	Marker string
	Label  string
}

func (s *Server) relatedEntriesGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx := s.active()
	if cx == nil || cx.DB == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	rels, err := relations.LoadImageRelations(cx.DB, id)
	if err != nil {
		logx.Warnf("related entries load %d: %v", id, err)
		http.Error(w, "load relations", http.StatusInternalServerError)
		return
	}
	siblings, sErr := loadCollectionSiblings(cx, id)
	if sErr != nil {
		logx.Warnf("collection siblings load %d: %v", id, sErr)
	}
	if len(siblings) > relatedPanelCap {
		siblings = siblings[:relatedPanelCap]
	}
	tiles := flattenRelationsForPanel(rels, id)
	paths := loadImagePaths(r.Context(), cx.DB, cx.Boundary(), id)
	back := parseBackContext(r)
	s.renderTemplate(w, "partials/related_entries.html", map[string]any{
		"ImageID":       id,
		"Tiles":         tiles,
		"Collection":    siblings,
		"ImagePaths":    paths,
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"ActiveGallery": s.activeGallery(),
		"BackQ":         back.Q,
		"BackSort":      back.Sort,
		"BackOrder":     back.Order,
		"BackSeed":      back.Seed,
		"BackPage":      back.Page,
	})
}

const relatedPanelCap = 6

type collectionSibling struct {
	ID     int64
	Series string
	Order  *int64
}

func loadCollectionSiblings(cx *galleryCtx, imageID int64) ([]collectionSibling, error) {
	rows, err := cx.DB.Read.Query(
		`SELECT jc.image_id, jc.name, jc.position
		 FROM image_collections self
		 JOIN image_collections jc ON jc.name = self.name
		 JOIN images i ON i.id = jc.image_id
		 WHERE self.image_id = ? AND jc.image_id != ? AND i.is_missing = 0
		 ORDER BY jc.name, jc.position IS NULL, jc.position, jc.image_id
		 LIMIT 200`,
		imageID, imageID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []collectionSibling
	for rows.Next() {
		var sib collectionSibling
		var ord sql.NullInt64
		if scanErr := rows.Scan(&sib.ID, &sib.Series, &ord); scanErr != nil {
			return nil, scanErr
		}
		if ord.Valid {
			v := ord.Int64
			sib.Order = &v
		}
		out = append(out, sib)
	}
	return out, rows.Err()
}

func flattenRelationsForPanel(rels *relations.ImageRelations, self int64) []relatedTile {
	var tiles []relatedTile
	seen := map[int64]bool{self: true}
	add := func(t relatedTile) {
		if len(tiles) >= relatedPanelCap || seen[t.ID] {
			return
		}
		seen[t.ID] = true
		tiles = append(tiles, t)
	}
	if rels.DupGroup != nil {
		for _, m := range rels.DupGroup.Members {
			if m == self {
				continue
			}
			marker := "Duplicate"
			label := "duplicate"
			if m == rels.DupGroup.Original {
				marker = "Original"
				label = "original"
			}
			add(relatedTile{ID: m, Marker: marker, Label: label})
		}
	}
	for _, m := range rels.AltGroupMembers {
		add(relatedTile{ID: m, Marker: "Alternate", Label: "alternate"})
	}
	if rels.VersionParent != nil {
		add(relatedTile{ID: *rels.VersionParent, Marker: "Earlier", Label: "previous version"})
	}
	if rels.VersionChild != nil {
		add(relatedTile{ID: *rels.VersionChild, Marker: "Newer", Label: "newer version"})
	}
	for _, m := range rels.DerivativeSources {
		add(relatedTile{ID: m, Marker: "Source", Label: "source"})
	}
	for _, m := range rels.Derivatives {
		add(relatedTile{ID: m, Marker: "Derivative", Label: "derivative"})
	}
	return tiles
}

func (s *Server) imageRelationsPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	img, err := loadImage(r.Context(), cx.DB, id)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}
	rels, err := relations.LoadImageRelations(cx.DB, id)
	if err != nil {
		logx.Warnf("relations page load %d: %v", id, err)
		http.Error(w, "load relations", http.StatusInternalServerError)
		return
	}
	siblings, sErr := loadCollectionSiblings(cx, id)
	if sErr != nil {
		logx.Warnf("relations page collection siblings %d: %v", id, sErr)
	}
	// Siblings already shown in a relation section above stay: dropping
	// them would leave gaps in the collection's numbering.
	collection := collectionWithSelf(siblings, *img)
	var nextOriginal int64
	if rels.DupGroup != nil && len(rels.DupGroup.Members) >= 3 {
		next, nErr := cx.RelationsSvc.NextOriginalIfRemoved(rels.DupGroup.ID, rels.DupGroup.Original)
		if nErr != nil {
			logx.Warnf("relations page next-original %d: %v", id, nErr)
		}
		nextOriginal = next
	}
	thumbURL := fmt.Sprintf("/thumbnails/%s/%d.jpg", s.activeGallery(), id)
	titleName := filepath.Base(img.CanonicalPath)
	if parent := filepath.Base(filepath.Dir(img.CanonicalPath)); parent != "" && parent != "." && parent != "/" {
		titleName = parent + "/" + titleName
	}
	back := parseBackContext(r)
	pageData := relationsImagePageData{
		baseData:                       s.base(r, "gallery", fmt.Sprintf("Relations - %s - %s", titleName, s.booruName())),
		Image:                          *img,
		Relations:                      rels,
		Self:                           id,
		Collection:                     collection,
		ThumbnailURL:                   thumbURL,
		NextOriginalIfOriginalUnlinked: nextOriginal,
		AltGroupMembersOrdered:         reorderSelfFirst(rels.AltGroupMembers, id),
		BackQ:                          back.Q,
		BackSort:                       back.Sort,
		BackOrder:                      back.Order,
		BackSeed:                       back.Seed,
		BackPage:                       back.Page,
	}
	if rels.DupGroup != nil {
		pageData.DupGroupMembersOrdered = reorderSelfFirst(rels.DupGroup.Members, id)
	}
	if rels.VersionParent != nil || rels.VersionChild != nil {
		gens, vErr := versionChainGensForImage(cx, id)
		if vErr != nil {
			logx.Warnf("relations page version chain %d: %v", id, vErr)
		}
		pageData.VersionChainGens = gens
		pageData.VersionActions = versionActionMap(id, rels)
	}
	if len(rels.DerivativeSources) > 0 || len(rels.Derivatives) > 0 {
		graph, tErr := derivativeGraphForImage(cx, id)
		if tErr != nil {
			logx.Warnf("relations page derivative graph %d: %v", id, tErr)
		}
		pageData.DerivativeGraph = graph
		pageData.DerivativeActions = derivativeActionMap(id, rels)
	}
	s.renderTemplate(w, "relations_image.html", pageData)
}

func reorderSelfFirst(members []int64, self int64) []int64 {
	out := slices.Clone(members)
	if i := slices.Index(out, self); i > 0 {
		out = append([]int64{self}, slices.Delete(out, i, i+1)...)
	}
	return out
}

func versionChainGensForImage(cx *galleryCtx, imageID int64) ([][]int64, error) {
	root, err := relations.ChainRoot(cx.DB.Read, "version_edges", "parent_image_id", "child_image_id", imageID)
	if err != nil {
		return nil, err
	}
	members := []int64{root}
	cur := root
	for i := 0; i < relations.MaxVersionChainDepth; i++ {
		var c int64
		err := cx.DB.Read.QueryRow(`SELECT child_image_id FROM version_edges WHERE parent_image_id = ?`, cur).Scan(&c)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		members = append(members, c)
		cur = c
	}
	if len(members) <= 1 {
		return nil, nil
	}
	gens := make([][]int64, len(members))
	for i, m := range members {
		gens[i] = []int64{m}
	}
	return gens, nil
}

func derivativeGraphForImage(cx *galleryCtx, imageID int64) (*derivGraph, error) {
	members, err := relations.DerivativeComponent(cx.DB.Read, imageID)
	if err != nil || len(members) < 2 {
		return nil, err
	}
	sourcesOf := map[int64][]int64{}
	for _, id := range members {
		srcs, err := db.QueryIDs(cx.DB.Read,
			`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ? ORDER BY source_image_id`, id)
		if err != nil {
			return nil, err
		}
		sourcesOf[id] = srcs
	}
	return layOutDerivatives(members, sourcesOf), nil
}

func collectionWithSelf(siblings []collectionSibling, self models.Image) []collectionSibling {
	if len(siblings) == 0 {
		return siblings
	}
	var order *int64
	if self.SeriesOrder != nil {
		v := int64(*self.SeriesOrder)
		order = &v
	}
	merged := make([]collectionSibling, 0, len(siblings)+1)
	merged = append(merged, siblings...)
	merged = append(merged, collectionSibling{ID: self.ID, Series: self.Series, Order: order})
	sort.SliceStable(merged, func(i, j int) bool {
		a, b := merged[i], merged[j]
		if !strings.EqualFold(a.Series, b.Series) {
			return strings.ToLower(a.Series) < strings.ToLower(b.Series)
		}
		aNull, bNull := a.Order == nil, b.Order == nil
		if aNull != bNull {
			return !aNull
		}
		if !aNull && !bNull && *a.Order != *b.Order {
			return *a.Order < *b.Order
		}
		return a.ID < b.ID
	})
	return merged
}

type relationsImagePageData struct {
	baseData
	Image                          models.Image
	Relations                      *relations.ImageRelations
	Self                           int64
	Collection                     []collectionSibling
	ThumbnailURL                   string
	NextOriginalIfOriginalUnlinked int64
	DupGroupMembersOrdered         []int64
	AltGroupMembersOrdered         []int64
	VersionChainGens               [][]int64
	DerivativeGraph                *derivGraph
	DerivativeActions              map[int64]string
	VersionActions                 map[int64]string
	BackQ                          string
	BackSort                       string
	BackOrder                      string
	BackSeed                       string
	BackPage                       string
}

func derivativeActionMap(self int64, rels *relations.ImageRelations) map[int64]string {
	m := map[int64]string{self: "this"}
	for _, s := range rels.DerivativeSources {
		m[s] = "source"
	}
	for _, d := range rels.Derivatives {
		m[d] = "derivative"
	}
	return m
}

func versionActionMap(self int64, rels *relations.ImageRelations) map[int64]string {
	m := map[int64]string{self: "this"}
	if rels.VersionParent != nil {
		m[*rels.VersionParent] = "earlier"
	}
	if rels.VersionChild != nil {
		m[*rels.VersionChild] = "newer"
	}
	return m
}

func (s *Server) recomputePhashPost(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	h, err := gallery.RecomputeAndStorePhash(r.Context(), cx.DB, id, cx.ThumbnailsPath)
	if err != nil {
		logx.Warnf("recompute phash %d: %v", id, err)
		// Flash at 200: htmx ignores HX-Trigger on a non-2xx response.
		// No refresh here, or the reload would wipe the flash.
		setFlashHeader(w, "phash recompute failed (is the thumbnail present?)", "err", nil)
		return
	}
	relations.PhashStored(cx.DB, id, h)
	cx.InvalidatePhashMissing()
	hxDone(w, r, "phash recomputed.", "", "/images/"+strconv.FormatInt(id, 10))
}

func (s *Server) relationsForm(w http.ResponseWriter, r *http.Request) (*galleryCtx, bool) {
	if !parseFormOK(w, r) {
		return nil, false
	}
	cx := s.active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return nil, false
	}
	return cx, true
}

func (s *Server) addRelationPost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	a, b, ok := parseRelationPair(w, r)
	if !ok {
		return
	}
	if r.FormValue("direction") == "ba" {
		a, b = b, a
	}
	relType := r.FormValue("type")
	add, known := relationAdders[relType]
	if !known {
		flashStatus(w, http.StatusBadRequest, "Unknown relation type.")
		return
	}
	var err error
	if r.FormValue("force") == "true" {
		err = cx.RelationsSvc.Overwrite(relType, a, b)
	} else {
		err = add(cx.RelationsSvc, a, b)
	}
	if err != nil {
		writeRelationError(w, err)
		return
	}
	cx.InvalidateCaches()
	setFlashHeader(w, "Relation added.", "ok", nil)
	writeInlineFlash(w, "ok", "Relation added.")
}

var relationAdders = map[string]func(svc *relations.Service, a, b int64) error{
	"duplicate":   (*relations.Service).AddDuplicate,
	"alternate":   (*relations.Service).AddAlternate,
	"version":     (*relations.Service).AddVersionEdge,
	"derivative":  (*relations.Service).AddDerivativeEdge,
	"not_related": (*relations.Service).AddNotRelated,
}

type relationRemoveOp struct {
	field string
	msg   string
	run   func(svc *relations.Service, a, b int64) error
}

var relationRemoveOps = map[string]relationRemoveOp{
	"duplicate": {field: "image_id", msg: "Relation removed.",
		run: func(svc *relations.Service, id, _ int64) error { return svc.RemoveDupMember(id) }},
	"alternate": {field: "image_id", msg: "Relation removed.",
		run: func(svc *relations.Service, id, _ int64) error { return svc.RemoveAltMember(id) }},
	"version":     {msg: "Relation removed.", run: (*relations.Service).RemoveVersionEdge},
	"derivative":  {msg: "Relation removed.", run: (*relations.Service).RemoveDerivativeEdge},
	"not_related": {msg: "Relation removed.", run: (*relations.Service).RemoveNotRelated},
	"dissolve-dup": {field: "group_id", msg: "Group dissolved.",
		run: func(svc *relations.Service, gid, _ int64) error { return svc.DissolveDupGroup(gid) }},
	"dissolve-alt": {field: "group_id", msg: "Group dissolved.",
		run: func(svc *relations.Service, gid, _ int64) error { return svc.DissolveAltGroup(gid) }},
	"dissolve-version": {field: "root_id", msg: "Version chain dissolved.",
		run: func(svc *relations.Service, rid, _ int64) error { return svc.DissolveVersionChain(rid) }},
	"dissolve-derivative": {field: "root_id", msg: "Derivative tree dissolved.",
		run: func(svc *relations.Service, rid, _ int64) error { return svc.DissolveDerivativeTree(rid) }},
}

func (s *Server) removeRelationPost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	relType := r.FormValue("type")
	var msg string
	switch op, tabled := relationRemoveOps[relType]; {
	case tabled:
		var a, b int64
		var ok bool
		if op.field != "" {
			a, ok = formInt64(w, r, op.field)
		} else {
			a, b, ok = parseRelationPair(w, r)
		}
		if !ok {
			return
		}
		if err := op.run(cx.RelationsSvc, a, b); err != nil {
			writeRelationError(w, err)
			return
		}
		msg = op.msg
	case relType == "promote-original":
		gid, ok := formInt64(w, r, "group_id")
		if !ok {
			return
		}
		id, ok := formInt64(w, r, "image_id")
		if !ok {
			return
		}
		if err := cx.RelationsSvc.PromoteToOriginal(gid, id); err != nil {
			writeRelationError(w, err)
			return
		}
		msg = "Original updated."
	case relType == "review-again":
		reviewAgainPost(w, r, cx)
		return
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown relation type.")
		return
	}
	cx.InvalidateCaches()
	setFlashHeader(w, msg, "ok", nil)
	writeInlineFlash(w, "ok", msg)
}

var reviewAgainRemovers = map[string]func(*relations.Service, int64, int64) error{
	"version":     (*relations.Service).RemoveVersionEdge,
	"derivative":  (*relations.Service).RemoveDerivativeEdge,
	"not_related": (*relations.Service).RemoveNotRelated,
}

func reviewAgainPost(w http.ResponseWriter, r *http.Request, cx *galleryCtx) {
	subkind := r.FormValue("kind")
	var a, b int64
	switch subkind {
	case "duplicate", "alternate":
		gid, ok := formInt64(w, r, "group_id")
		if !ok {
			return
		}
		// A group dissolved since the page rendered still aggregates to
		// one row, of NULLs.
		var n int
		var ar, br int64
		if err := cx.DB.Read.QueryRow(
			`SELECT COUNT(*), COALESCE(MIN(image_id), 0), COALESCE(MAX(image_id), 0)
			 FROM `+groupMembersTable(subkind)+` WHERE group_id = ?`, gid,
		).Scan(&n, &ar, &br); err != nil {
			writeRelationError(w, err)
			return
		}
		if n != 2 {
			flashStatus(w, http.StatusBadRequest, "Group must have exactly two members.")
			return
		}
		a, b = ar, br
		if subkind == "duplicate" {
			if err := cx.RelationsSvc.DissolveDupGroup(gid); err != nil {
				writeRelationError(w, err)
				return
			}
		} else {
			if err := cx.RelationsSvc.DissolveAltGroup(gid); err != nil {
				writeRelationError(w, err)
				return
			}
		}
	case "version", "derivative", "not_related":
		ar, br, ok := parseRelationPair(w, r)
		if !ok {
			return
		}
		if err := reviewAgainRemovers[subkind](cx.RelationsSvc, ar, br); err != nil {
			writeRelationError(w, err)
			return
		}
		a, b = ar, br
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown review-again kind.")
		return
	}
	if err := cx.RelationsSvc.RemoveNotRelated(a, b); err != nil {
		writeRelationError(w, err)
		return
	}
	if err := cx.RelationsSvc.QueueForReview(a, b); err != nil {
		writeRelationError(w, err)
		return
	}
	cx.InvalidateCaches()
	// Pinned to the pair, or the session opens on whatever the queue
	// sorts first.
	lo, hi := min(a, b), max(a, b)
	dest := "/relations/session?a=" + strconv.FormatInt(lo, 10) + "&b=" + strconv.FormatInt(hi, 10)
	hxRedirect(w, r, dest)
}

func groupMembersTable(subkind string) string {
	if subkind == "alternate" {
		return "alt_group_members"
	}
	return "dup_group_members"
}

type copyTagsPreviewGroup struct {
	Category string
	Color    string
	Tags     []string
}

func (s *Server) copyTagsToOriginalPreview(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	gid, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	var original int64
	if err := cx.DB.Read.QueryRow(`SELECT original_image_id FROM dup_groups WHERE id = ?`, gid).Scan(&original); err != nil {
		http.NotFound(w, r)
		return
	}
	rows, err := cx.DB.Read.Query(`
		SELECT DISTINCT t.name, COALESCE(c.name, ''), COALESCE(c.color, '')
		FROM image_tags it
		JOIN dup_group_members m ON m.image_id = it.image_id
		JOIN tags t ON t.id = it.tag_id
		LEFT JOIN tag_categories c ON c.id = t.category_id
		WHERE m.group_id = ?
		  AND m.image_id != ?
		  AND it.is_implied = 0
		  AND (c.name IS NULL OR c.name != 'rating')
		  AND NOT EXISTS (
		    SELECT 1 FROM image_tags it2 WHERE it2.image_id = ? AND it2.tag_id = it.tag_id
		  )
		ORDER BY c.name, t.name`,
		gid, original, original,
	)
	if err != nil {
		http.Error(w, "load preview", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rows.Close() }()
	type row struct {
		name     string
		category string
		color    string
	}
	var entries []row
	for rows.Next() {
		var rec row
		if scanErr := rows.Scan(&rec.name, &rec.category, &rec.color); scanErr != nil {
			http.Error(w, "scan preview", http.StatusInternalServerError)
			return
		}
		entries = append(entries, rec)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "load preview", http.StatusInternalServerError)
		return
	}
	category := func(e row) string { return cmp.Or(e.category, "(uncategorised)") }
	groups := groupOrdered(entries, nil, category,
		func(e row) *copyTagsPreviewGroup {
			return &copyTagsPreviewGroup{Category: category(e), Color: e.color}
		},
		func(g *copyTagsPreviewGroup, e row) { g.Tags = append(g.Tags, e.name) })
	total := 0
	for _, g := range groups {
		total += len(g.Tags)
	}
	s.renderTemplate(w, "partials/copy_tags_preview.html", map[string]any{
		"GroupID":    gid,
		"OriginalID": original,
		"Groups":     groups,
		"Total":      total,
		"CSRFToken":  s.csrfToken(sessionFromContext(r.Context())),
	})
}

func (s *Server) reverseRelationPost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	a, b, ok := parseRelationPair(w, r)
	if !ok {
		return
	}
	switch r.FormValue("type") {
	case "version":
		if err := cx.RelationsSvc.ReverseVersionEdge(a, b); err != nil {
			writeRelationError(w, err)
			return
		}
	case "derivative":
		if err := cx.RelationsSvc.ReverseDerivativeEdge(a, b); err != nil {
			writeRelationError(w, err)
			return
		}
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown relation type.")
		return
	}
	cx.InvalidateCaches()
	writeInlineFlash(w, "ok", "Edge reversed.")
}

func (s *Server) mergeGroupsPost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	kind = cmp.Or(kind, r.FormValue("kind"))
	if kind != "alt" && kind != "dup" {
		flashStatus(w, http.StatusBadRequest, "Unknown merge kind.")
		return
	}
	ids := parseIDList(r.Form["group_id"])
	if len(ids) < 2 {
		flashStatus(w, http.StatusBadRequest, "Pick at least two groups to merge.")
		return
	}
	switch kind {
	case "alt":
		if err := cx.RelationsSvc.MergeAltGroups(ids); err != nil {
			writeRelationError(w, err)
			return
		}
	case "dup":
		var keep int64
		if raw := strings.TrimSpace(r.FormValue("keep_original_from")); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				flashStatus(w, http.StatusBadRequest, "Invalid keep_original_from.")
				return
			}
			if !slices.Contains(ids, v) {
				flashStatus(w, http.StatusBadRequest, "keep_original_from must be one of the merging groups.")
				return
			}
			keep = v
		}
		if err := cx.RelationsSvc.MergeDupGroups(ids, keep); err != nil {
			writeRelationError(w, err)
			return
		}
	}
	cx.InvalidateCaches()
	redirectKind := "duplicate"
	if kind == "alt" {
		redirectKind = "alternate"
	}
	target := "/relations/browse?kind=" + redirectKind
	hxRedirect(w, r, target)
}

func (s *Server) dissolveGroupsPost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	kind = cmp.Or(kind, r.FormValue("kind"))
	byRoot := map[string]struct {
		field    string
		dissolve func(int64) error
	}{
		"duplicate":  {"group_id", cx.RelationsSvc.DissolveDupGroup},
		"alternate":  {"group_id", cx.RelationsSvc.DissolveAltGroup},
		"version":    {"root_id", cx.RelationsSvc.DissolveVersionChain},
		"derivative": {"root_id", cx.RelationsSvc.DissolveDerivativeTree},
	}
	switch entry, uniform := byRoot[kind]; {
	case uniform:
		for _, id := range parseIDList(r.Form[entry.field]) {
			if err := entry.dissolve(id); err != nil {
				writeRelationError(w, err)
				return
			}
		}
	case kind == "not_related":
		for _, raw := range r.Form["pair"] {
			a, b, ok := parsePairValue(raw)
			if !ok {
				continue
			}
			if err := cx.RelationsSvc.RemoveNotRelated(a, b); err != nil {
				writeRelationError(w, err)
				return
			}
		}
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown dissolve kind.")
		return
	}
	cx.InvalidateCaches()
	target := "/relations/browse?kind=" + kind
	hxRedirect(w, r, target)
}

func parseIDList(raw []string) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(raw))
	for _, field := range raw {
		// One id per field or comma-joined: htmx flattens arrays that
		// way, and the batch bar joins its selection to stay under
		// net/url's 10000-parameter cap.
		for _, s := range strings.Split(field, ",") {
			v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func parsePairValue(raw string) (int64, int64, bool) {
	colon := strings.IndexByte(raw, ':')
	if colon <= 0 || colon == len(raw)-1 {
		return 0, 0, false
	}
	a, err := strconv.ParseInt(strings.TrimSpace(raw[:colon]), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	b, err := strconv.ParseInt(strings.TrimSpace(raw[colon+1:]), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return a, b, true
}

func (s *Server) copyTagsToOriginalPost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	gid, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	added, err := cx.RelationsSvc.CopyTagsFromDuplicatesToOriginal(gid)
	if err != nil {
		writeRelationError(w, err)
		return
	}
	cx.InvalidateCaches()
	writeInlineFlash(w, "ok", fmt.Sprintf("Copied %d tag(s) to the original.", added))
}

func parseRelationPair(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	a, ok := formInt64(w, r, "a")
	if !ok {
		return 0, 0, false
	}
	b, ok := formInt64(w, r, "b")
	if !ok {
		return 0, 0, false
	}
	if a == b {
		// 200, or htmx would not swap the message into its target.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		flashStatus(w, http.StatusOK, "Cannot relate an image to itself.")
		return 0, 0, false
	}
	return a, b, true
}

// writeRelationError answers 200 because htmx does not swap a 4xx body
// into the target.
func writeRelationError(w http.ResponseWriter, err error) {
	msg := err.Error()
	if fe := relations.FriendlyErrorFor(err); fe != nil {
		msg = fe.Message
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	flashStatus(w, http.StatusOK, msg)
}
