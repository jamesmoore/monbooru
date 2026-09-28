package web

import (
	"cmp"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

type tagsPageData struct {
	baseData
	Tags         []models.Tag
	Categories   []models.TagCategory
	Implications map[int64][]models.Implication
	Total        int
	Page         int
	TotalPages   int
	CategoryID   string
	Prefix       string
	Sort         string
	Order        string
	Origin       string
	Type         string
	// Raw, not the resolved cutoff, so the sidebar chips can match it.
	CreatedAfter    string
	Conflicts       bool
	ConflictsTotal  int
	Stale           string
	StaleTotal      int
	FullyStaleTotal int
	Folded          bool
	FoldedTotal     int
	OriginCounts    []tags.OriginCount
	UsedBy          string
	UsedByLabels    []string
	UsedBySources   map[int64][]string
	OriginKinds     map[string]string
	ShowZero        bool
	ZeroOnly        bool
	BackQS          string
}

func (s *Server) originKinds(labels []string) map[string]string {
	kinds := make(map[string]string, len(labels))
	var unknown []string
	for _, l := range labels {
		switch l {
		case "":
		case "user":
			kinds[l] = "user"
		case "ptr":
			kinds[l] = "ptr"
		case "auto":
			kinds[l] = "auto"
		default:
			unknown = append(unknown, l)
		}
	}
	if len(unknown) > 0 {
		autoSet, err := s.tagSvc().AutoTaggerLabels(unknown)
		if err != nil {
			logx.Warnf("classify origin labels: %v", err)
		}
		for _, l := range unknown {
			if _, ok := autoSet[l]; ok {
				kinds[l] = "auto"
			} else {
				kinds[l] = "site"
			}
		}
	}
	return kinds
}

func createdAfterCutoff(raw string) string {
	now := time.Now().UTC()
	switch raw {
	case "":
		return ""
	case "24h":
		return now.Add(-24 * time.Hour).Format(time.RFC3339)
	case "7d":
		return now.AddDate(0, 0, -7).Format(time.RFC3339)
	case "30d":
		return now.AddDate(0, 0, -30).Format(time.RFC3339)
	}
	return raw
}

type tagsSidebarCounts struct {
	Conflicts  int
	Stale      int
	FullyStale int
	Folded     int
	Origins    []tags.OriginCount
	UsedBy     []string
	Err        error
}

// In parallel: in sequence the scans cost more than the listing they decorate.
func (s *Server) tagsSidebarLoad(typeFilter string) tagsSidebarCounts {
	var c tagsSidebarCounts
	var mu sync.Mutex
	var wg sync.WaitGroup
	svc := s.tagSvc()
	run := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				mu.Lock()
				c.Err = cmp.Or(c.Err, err)
				mu.Unlock()
			}
		}()
	}
	run(func() (err error) { c.Conflicts, err = svc.ConflictsCount(); return })
	run(func() (err error) { c.Stale, err = svc.StaleUsageCount(); return })
	run(func() (err error) { c.FullyStale, err = svc.FullyStaleCount(); return })
	run(func() (err error) { c.Folded, err = svc.FoldedDuplicatesCount(); return })
	run(func() (err error) { c.Origins, err = svc.OriginCounts(typeFilter); return })
	run(func() (err error) { c.UsedBy, err = svc.UsedByLabels(); return })
	wg.Wait()
	return c
}

func (s *Server) tagsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	catIDStr := q.Get("cat")
	prefix := q.Get("q")
	if catIDStr == "" && prefix != "" && strings.HasSuffix(prefix, ":") && strings.Count(prefix, ":") == 1 {
		catName := strings.TrimSuffix(prefix, ":")
		if catName != "" && s.categoryExists(catName) {
			if catID, ok, err := tags.CategoryIDByName(s.db(), catName); ok && err == nil {
				dst := r.URL
				vals := dst.Query()
				vals.Del("q")
				vals.Set("cat", strconv.FormatInt(catID, 10))
				dst.RawQuery = vals.Encode()
				http.Redirect(w, r, dst.String(), http.StatusSeeOther)
				return
			}
		}
	}
	p := tagListingParamsFrom(q)

	tagList, total, err := s.tagSvc().ListTags(s.tagListingFilter(p))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cats, _ := s.tagSvc().ListCategories()
	totalPages := (total + 99) / 100

	if total > 0 && p.Page > totalPages {
		p.Page = totalPages
		tagList, total, err = s.tagSvc().ListTags(s.tagListingFilter(p))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	parentIDs := make([]int64, 0, len(tagList))
	for _, t := range tagList {
		if !t.IsAlias {
			parentIDs = append(parentIDs, t.ID)
		}
	}
	imps, err := s.tagSvc().ImplicationsForParents(parentIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	counts := s.tagsSidebarLoad(p.Type)
	if counts.Err != nil {
		http.Error(w, counts.Err.Error(), http.StatusInternalServerError)
		return
	}
	rowIDs := make([]int64, 0, len(tagList))
	for _, t := range tagList {
		rowIDs = append(rowIDs, t.ID)
	}
	usedBySources, err := s.tagSvc().UsedByForTags(rowIDs, counts.UsedBy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pageLabels := make([]string, 0, len(tagList)+len(counts.Origins)+len(counts.UsedBy))
	for _, t := range tagList {
		pageLabels = append(pageLabels, t.Origin)
	}
	for _, oc := range counts.Origins {
		pageLabels = append(pageLabels, oc.Label)
	}
	pageLabels = append(pageLabels, counts.UsedBy...)

	data := tagsPageData{
		baseData:        s.base(r, "tags", "Tags - "+s.booruName()),
		Tags:            tagList,
		Categories:      cats,
		Implications:    imps,
		Total:           total,
		Page:            p.Page,
		TotalPages:      totalPages,
		CategoryID:      p.CatID,
		Prefix:          p.Prefix,
		Sort:            p.Sort,
		Order:           p.Order,
		Origin:          p.Origin,
		Type:            p.Type,
		CreatedAfter:    p.CreatedAfter,
		Conflicts:       p.Conflicts,
		ConflictsTotal:  counts.Conflicts,
		Stale:           p.Stale,
		StaleTotal:      counts.Stale,
		FullyStaleTotal: counts.FullyStale,
		Folded:          p.Folded,
		FoldedTotal:     counts.Folded,
		OriginCounts:    counts.Origins,
		UsedBy:          p.UsedBy,
		UsedByLabels:    counts.UsedBy,
		UsedBySources:   usedBySources,
		OriginKinds:     s.originKinds(pageLabels),
		ShowZero:        p.ShowZero,
		ZeroOnly:        p.ZeroOnly,
		BackQS:          p.backQS(),
	}
	s.renderTemplate(w, "tags.html", data)
}

type tagListingParams struct {
	CatID, Prefix, Sort, Order, Origin, Type, CreatedAfter, ZeroParam, Stale, UsedBy string
	HasType, Conflicts, ShowZero, ZeroOnly, Folded                                   bool
	Page                                                                             int
}

func tagListingParamsFrom(q url.Values) tagListingParams {
	p := tagListingParams{
		CatID:        q.Get("cat"),
		Prefix:       q.Get("q"),
		Sort:         q.Get("sort"),
		Order:        q.Get("order"),
		Origin:       q.Get("origin"),
		UsedBy:       q.Get("used_by"),
		Type:         q.Get("type"),
		HasType:      q.Has("type"),
		CreatedAfter: q.Get("created_after"),
		Conflicts:    q.Get("conflicts") == "1",
		ZeroParam:    q.Get("show_zero"),
		Page:         1,
	}
	if s := q.Get("stale"); s == "has" || s == "full" {
		p.Stale = s
	}
	p.Folded = q.Get("folded") == "1"
	p.Sort = cmp.Or(p.Sort, "usage")
	if p.Order != "asc" && p.Order != "desc" {
		switch p.Sort {
		case "usage", "created", "last_used":
			p.Order = "desc"
		default:
			p.Order = "asc"
		}
	}
	// An absent type means plain tags; an empty type= is All.
	// origin=alias selects alias rows, which type=tag would hide.
	if !p.HasType && p.Origin != "alias" {
		p.Type = "tag"
	}
	p.ZeroOnly = p.ZeroParam == "only"
	p.ShowZero = p.ZeroOnly || p.ZeroParam != "0"
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 0 {
		p.Page = n
	}
	return p
}

func (s *Server) tagListingFilter(p tagListingParams) tags.TagFilter {
	f := s.buildTagFilter(p.CatID, p.Prefix, p.Sort, p.Order, p.Origin, p.Type, p.CreatedAfter, p.ShowZero, p.ZeroOnly, p.Page, 100)
	f.ConflictsOnly = p.Conflicts
	f.Stale = p.Stale
	f.FoldedOnly = p.Folded
	f.UsedBy = p.UsedBy
	return f
}

func (p tagListingParams) backQS() string {
	v := url.Values{}
	if p.Prefix != "" {
		v.Set("q", p.Prefix)
	}
	v.Set("sort", p.Sort)
	v.Set("order", p.Order)
	if p.HasType || p.Type != "" {
		v.Set("type", p.Type)
	}
	if p.CatID != "" {
		v.Set("cat", p.CatID)
	}
	if p.Origin != "" {
		v.Set("origin", p.Origin)
	}
	if p.UsedBy != "" {
		v.Set("used_by", p.UsedBy)
	}
	if p.CreatedAfter != "" {
		v.Set("created_after", p.CreatedAfter)
	}
	if p.Conflicts {
		v.Set("conflicts", "1")
	}
	if p.Stale != "" {
		v.Set("stale", p.Stale)
	}
	if p.Folded {
		v.Set("folded", "1")
	}
	if p.ZeroParam != "" {
		v.Set("show_zero", p.ZeroParam)
	}
	if p.Page > 1 {
		v.Set("page", strconv.Itoa(p.Page))
	}
	return v.Encode()
}

func (s *Server) buildTagFilter(catIDStr, prefix, sortStr, orderStr, originStr, typeStr, createdAfterRaw string, showZero, zeroOnly bool, page, limit int) tags.TagFilter {
	f := tags.TagFilter{
		Prefix:       prefix,
		Sort:         sortStr,
		Order:        orderStr,
		PageIndex:    page - 1,
		Limit:        limit,
		Origin:       originStr,
		Type:         typeStr,
		CreatedAfter: createdAfterCutoff(createdAfterRaw),
		ShowZero:     showZero,
		ZeroOnly:     zeroOnly,
	}
	if catIDStr != "" {
		if id, err := strconv.ParseInt(catIDStr, 10, 64); err == nil {
			f.CategoryID = &id
		} else if id, ok := s.categoryIDByName(catIDStr); ok {
			f.CategoryID = &id
		}
	}
	return f
}

// "#<id>" is a tag id and anything else a name: normalised, an alias
// followed to its canonical, a bare one taken only when it names a single
// tag across the categories.
func (s *Server) resolveCanonicalTagInput(input string, create bool) (int64, string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, "Tag name is required."
	}
	if digits, ok := strings.CutPrefix(input, "#"); ok {
		var exists int
		id, err := strconv.ParseInt(digits, 10, 64)
		if err == nil {
			err = s.db().Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE id = ?`, id).Scan(&exists)
		}
		if err != nil || exists == 0 {
			return 0, "Tag not found: " + input
		}
		return id, ""
	}
	catName, bare := "", input
	if idx := strings.Index(input, ":"); idx > 0 && s.categoryExists(input[:idx]) {
		catName, bare = input[:idx], strings.TrimSpace(input[idx+1:])
		if bare == "" {
			return 0, "Tag name is required after the category prefix."
		}
	}
	name, err := tags.ValidateTagName(strings.Trim(bare, `"`))
	if err != nil {
		return 0, err.Error()
	}
	var catID int64
	if catName != "" {
		id, ok, err := tags.CategoryIDByName(s.db(), catName)
		if !ok || err != nil {
			return 0, "Category not found: " + catName
		}
		catID = id
	}
	type choice struct {
		id    int64
		label string
	}
	choices, err := db.QueryAll(s.db().Read, func(rows *sql.Rows) (choice, error) {
		var c choice
		err := rows.Scan(&c.id, &c.label)
		return c, err
	}, `SELECT DISTINCT ct.id, c.name || ':' || ct.name
	    FROM tags t
	    JOIN tags ct ON ct.id = COALESCE(t.canonical_tag_id, t.id)
	    JOIN tag_categories c ON c.id = ct.category_id
	    WHERE t.name = ? AND (? = 0 OR t.category_id = ?)
	    ORDER BY c.name`, name, catID, catID)
	if err != nil {
		return 0, "Tag lookup failed: " + err.Error()
	}
	switch len(choices) {
	case 1:
		return choices[0].id, ""
	case 0:
		if !create {
			return 0, "Tag not found: " + input
		}
		if catID == 0 {
			cx := s.active()
			if cx == nil || cx.GeneralCategoryID == 0 {
				return 0, "Could not resolve the general category."
			}
			catID = cx.GeneralCategoryID
		}
		tag, err := s.tagSvc().GetOrCreateTag(name, catID)
		if err != nil {
			return 0, err.Error()
		}
		return tag.ID, ""
	}
	labels := make([]string, len(choices))
	for i, c := range choices {
		labels[i] = c.label
	}
	return 0, "Tag name " + name + " exists in more than one category; use " + strings.Join(labels, " or ")
}

func (s *Server) qualifiedTagName(id int64) string {
	t, err := s.tagSvc().GetTag(id)
	if err != nil {
		return "#" + strconv.FormatInt(id, 10)
	}
	return t.CategoryName + ":" + t.Name
}

func (s *Server) createTagPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	catIDStr := r.FormValue("category_id")

	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		externalErr(w, r, "Invalid category.", http.StatusBadRequest)
		return
	}
	if _, err := s.tagSvc().GetOrCreateTag(name, catID); err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	s.active().InvalidateCaches()
	hxDone(w, r, "Tag "+name+" created.", "/tags?q="+url.QueryEscape(name), "/tags")
}

func (s *Server) createAliasPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	catIDStr := r.FormValue("category_id")
	canonInput := strings.TrimSpace(r.FormValue("canonical_id"))

	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		externalErr(w, r, "Invalid category.", http.StatusBadRequest)
		return
	}
	canonID, msg := s.resolveCanonicalTagInput(canonInput, true)
	if msg != "" {
		externalErr(w, r, msg, http.StatusBadRequest)
		return
	}

	if _, err := s.tagSvc().CreateAlias(name, catID, canonID); err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	s.active().InvalidateCaches()

	hxDone(w, r, "Alias "+name+" created, resolving to "+s.qualifiedTagName(canonID)+".", "/tags?type=alias&q="+url.QueryEscape(name), "/tags?type=alias")
}

func (s *Server) addTagAliasPost(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	rawInput := strings.TrimSpace(r.FormValue("name"))
	if rawInput == "" {
		writeInlineFlash(w, "err", "Alias name is required.")
		return
	}
	catTags, unknownCats, parseErrMsg := s.parseTagInput(rawInput)
	if parseErrMsg != "" {
		writeInlineFlash(w, "err", parseErrMsg)
		return
	}

	added := 0
	var failures []string
	for _, ct := range catTags {
		if _, err := s.tagSvc().CreateAlias(ct.name, ct.catID, id); err != nil {
			failures = append(failures, ct.name+": "+err.Error())
			continue
		}
		added++
	}
	if added > 0 {
		s.active().InvalidateCaches()
	}
	switch {
	case len(failures) == 0:
		noun := "alias"
		if added != 1 {
			noun = "aliases"
		}
		msg := strconv.Itoa(added) + " " + noun + " created."
		if note := unknownCategoryNote(unknownCats); note != "" {
			msg += " " + note + "."
		}
		hxDone(w, r, msg, "", fmt.Sprintf("/tags/%d", id))
	case added > 0:
		writeInlineFlash(w, "err", "Added "+strconv.Itoa(added)+". Failed: "+strings.Join(failures, "; "))
	default:
		writeInlineFlash(w, "err", strings.Join(failures, "; "))
	}
}

func (s *Server) removeTagAliasesDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	origin, stale := relationGroupFilter(r)
	byCanonical, err := s.tagSvc().AliasesForTagIDs([]int64{id})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	removed := 0
	for _, a := range byCanonical[id] {
		if a.Origin != origin || a.Stale != stale {
			continue
		}
		if err := s.tagSvc().DeleteTag(a.ID); err != nil {
			logx.Warnf("delete alias %d: %v", a.ID, err)
			continue
		}
		removed++
	}
	if removed > 0 {
		s.active().InvalidateCaches()
	}
	noun := "alias"
	if removed != 1 {
		noun = "aliases"
	}
	setFlashHeader(w, strconv.Itoa(removed)+" "+noun+" removed.", "ok",
		map[string]any{"tag-relations-changed": ""})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteTagHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	tag, _ := s.tagSvc().GetTag(id)
	if err := s.tagSvc().DeleteTag(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.active().InvalidateCaches()
	// tag-relations-changed makes the tag page's PTR panel refetch its diff.
	if tag != nil && tag.IsAlias {
		setFlashHeader(w, "Alias removed.", "ok", map[string]any{"tag-relations-changed": ""})
	} else {
		w.Header().Set("HX-Trigger", "tag-relations-changed")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteTagsSearchPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.startTagScopeRun(w, r, s.runDeleteTagsByIDs)
}

func (s *Server) runDeleteTagsByIDs(ids []int64) {
	deleted, skipped, reasons, cancelled := s.runTagScopeLoop(ids, "deleting tags…", 50, func(id int64) (bool, error) {
		if err := s.tagSvc().DeleteTag(id); err != nil {
			logx.Warnf("delete tag %d: %v", id, err)
			return false, err
		}
		return true, nil
	})

	s.active().InvalidateCaches()
	summary := skippedSuffix(fmt.Sprintf("deleted %d tag(s)", deleted), skipped)
	s.finishTagScopeJob(deleted, reasons, cancelled, "delete tags", summary)
}

func (s *Server) renameTagPost(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	newName, ok := requiredFormExternal(w, r, "name", "Name required.")
	if !ok {
		return
	}
	var err error
	if r.FormValue("keep_alias") == "1" {
		err = s.tagSvc().RenameTagKeepAlias(id, newName)
	} else {
		err = s.tagSvc().RenameTag(id, newName)
	}
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	// Cached id lists for ?q=oldname would outlive the rename.
	s.active().InvalidateCaches()
	// HX-Refresh, not a redirect: the listing keeps its filters.
	hxDone(w, r, "Renamed to "+newName+".", "", "/tags")
}
