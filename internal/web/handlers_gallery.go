package web

import (
	"cmp"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
)

// A first search slower than this skips the second count behind "N hidden".
var galleryHiddenIndicatorBudget = 300 * time.Millisecond

const batchGapMinutes = 15

type galleryData struct {
	baseData
	Query             string
	SearchWarning     string
	Sort              string
	Order             string
	ThumbnailFit      string
	ThumbSize         string
	RandomSeed        int64
	Page              int
	TotalPages        int
	PageSize          int
	PageSizeChoices   []PageSizeChoice
	StatusOOB         bool
	Result            *models.SearchResult
	SidebarTags       []models.Tag
	FolderTree        []gallery.FolderNode
	SourceLabelCounts []gallery.SourceLabelCount
	SavedSearches     []models.SavedSearch
	SidebarURL        string
	EnabledTaggers    []tagger.TaggerStatus
	TaggersPresent    bool
	TaggerReason      string
	ActiveTagTerms    map[string]bool
	InboxClusterAtIdx []*inboxCluster
	InboxUploadActive bool
	AcceptFileTypes   string
	SimilarityPercent map[int64]int
	SortSelectOOB     bool
	PluginSlot        pluginSlotView
}

type inboxCluster struct {
	Count      int
	DateLabel  string
	RangeLabel string
	RangeLink  string
	RangeQuery string
	Whole      bool // every row of the batch is on this page
}

func (s *Server) galleryHandler(w http.ResponseWriter, r *http.Request) {
	// The grid swap pushes this URL, so a Back that misses the bfcache
	// would otherwise be served the cached fragment as the page.
	w.Header().Add("Vary", "HX-Target")
	q := r.URL.Query()
	queryStr := q.Get("q")
	expr := search.Parse(queryStr)
	sortStr := q.Get("sort")
	orderStr := q.Get("order")
	if sortStr == "" && search.HasSimilarTerm(expr) {
		sortStr = "similarity"
	}
	// The executor falls back to newest too; this keeps the Sort select honest.
	if sortStr == "similarity" && !search.HasSimilarTerm(expr) {
		sortStr = "newest"
	}
	if sortStr == "" && orderStr == "" && collectionFilterActive(expr) {
		sortStr, orderStr = "order", "asc"
	}
	sortStr = cmp.Or(sortStr, "newest")
	orderStr = cmp.Or(orderStr, search.DefaultOrder(sortStr))
	pageStr := q.Get("page")
	page := 1
	pageNonPositive := false
	if pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil {
			if p > 0 {
				page = p
			} else {
				pageNonPositive = true
			}
		}
	}

	var randomSeed int64
	if sortStr == "random" {
		if seedStr := q.Get("seed"); seedStr != "" {
			if s, err := strconv.ParseInt(seedStr, 10, 64); err == nil && s != 0 {
				randomSeed = s
			}
		}
		if randomSeed == 0 {
			seedBytes := make([]byte, 8)
			if _, err := rand.Read(seedBytes); err == nil {
				randomSeed = int64(binary.BigEndian.Uint32(seedBytes) | 1)
			} else {
				randomSeed = time.Now().UnixNano() & 0x7FFFFFFF
			}
			if randomSeed < 0 {
				randomSeed = -randomSeed
			}
			newQ := r.URL.Query()
			newQ.Set("seed", strconv.FormatInt(randomSeed, 10))
			if isHTMXRequest(r) {
				// Push URL with seed so the next poll keeps the same order.
				w.Header().Set("HX-Push-Url", "/?"+newQ.Encode())
			} else {
				http.Redirect(w, r, "/?"+newQ.Encode(), http.StatusSeeOther)
				return
			}
		}
	}

	ceiling := resolveCeiling(r, s.active())
	pinnedCollection := search.PinnedCollectionName(expr)
	expr = ceiling.Apply(expr)
	pageSize := s.pageSize(r)
	sq := search.Query{
		Expr:       expr,
		Sort:       sortStr,
		Order:      orderStr,
		RandomSeed: randomSeed,
		Page:       page,
		Limit:      pageSize,
		CacheKey:   search.BuildAdjacencyCacheKey(s.activeGallery(), queryStr, sortStr, orderStr, randomSeed, ceiling.Level()),
	}
	if sortStr == "order" {
		sq.OrderCollection = pinnedCollection
	}
	// Checked after ceiling.Apply: the cached count ignores the ceiling.
	if expr == nil {
		if cx := s.active(); cx != nil {
			if n, ok := cx.VisibleCount(); ok {
				sq.PresetTotal = &n
			}
		}
	}

	htmxGridTarget := isHTMXRequest(r) && r.Header.Get("HX-Target") == "gallery-grid"

	firstStart := time.Now()
	result, err := search.Execute(s.db(), sq)
	if err != nil {
		logx.Errorf("gallery search: %v", err)
		http.Error(w, "search error", http.StatusInternalServerError)
		return
	}
	firstElapsed := time.Since(firstStart)

	totalPages := 1
	if pageSize > 0 {
		totalPages = (result.Total + pageSize - 1) / pageSize
	}

	if (result.Total > 0 && page > totalPages) || pageNonPositive {
		if page > totalPages {
			page = totalPages
			sq.Page = page
			result, err = search.Execute(s.db(), sq)
			if err != nil {
				logx.Errorf("gallery search (clamped): %v", err)
				http.Error(w, "search error", http.StatusInternalServerError)
				return
			}
		}
		if page < 1 {
			// No results leave totalPages at 0; a redirect to page=0 loops.
			page = 1
		}
		clampedQ := r.URL.Query()
		clampedQ.Set("page", strconv.Itoa(page))
		clampedURL := "/?" + clampedQ.Encode()
		if isHTMXRequest(r) {
			w.Header().Set("HX-Push-Url", clampedURL)
		} else {
			http.Redirect(w, r, clampedURL, http.StatusSeeOther)
			return
		}
	}

	hiddenByCeiling := 0
	if ceiling.IsActive() {
		rawTotal := -1
		bareExpr := search.Parse(queryStr)
		switch {
		case bareExpr == nil:
			if cx := s.active(); cx != nil {
				if n, ok := cx.VisibleCount(); ok {
					rawTotal = n
				}
			}
		case firstElapsed < galleryHiddenIndicatorBudget:
			bareKey := search.BuildAdjacencyCacheKey(s.activeGallery(), queryStr, sortStr, orderStr, randomSeed, "")
			if cachedIDs, ok := search.AdjacencyCacheGet(bareKey); ok {
				rawTotal = len(cachedIDs)
			} else {
				rawResult, err := search.Execute(s.db(), search.Query{
					Expr: bareExpr, Sort: sortStr, Order: orderStr,
					RandomSeed: randomSeed, Page: 1, Limit: 1,
				})
				if err == nil {
					rawTotal = rawResult.Total
				}
			}
		}
		if rawTotal > result.Total {
			hiddenByCeiling = rawTotal - result.Total
		}
	}

	// Only the uncollapsed HTMX fragment renders the sidebar inline; a full
	// page lazy-loads it so first paint doesn't wait on the sidebar reads.
	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
	}

	var sb sidebarBundle
	if htmxGridTarget && !sidebarCollapsed(r) {
		sb = s.sidebarLoad(ids, ceiling)
	}

	taggerCfg := s.cfgSnapshot()
	data := galleryData{
		baseData:          s.base(r, "gallery", "Images - "+s.booruName()),
		Query:             queryStr,
		SearchWarning:     searchWarning(expr),
		Sort:              sortStr,
		Order:             orderStr,
		ThumbnailFit:      s.thumbnailFit(),
		ThumbSize:         thumbSize(r),
		RandomSeed:        randomSeed,
		Page:              page,
		TotalPages:        totalPages,
		PageSize:          pageSize,
		PageSizeChoices:   s.pageSizeChoices(r),
		Result:            result,
		SidebarTags:       sb.Tags,
		FolderTree:        sb.Folders,
		SourceLabelCounts: sb.SourceLabels,
		SavedSearches:     sb.Saved,
		EnabledTaggers:    tagger.EnabledTaggersForGallery(taggerCfg, s.activeGallery()),
		TaggersPresent:    tagger.Present(taggerCfg),
		TaggerReason:      tagger.UnavailableReason(taggerCfg),
		PluginSlot:        s.pluginSlot(r, config.SlotBatchBar, 0, ""),
		ActiveTagTerms:    computeActiveTagTerms(queryStr),
	}
	if seedID, ok := search.SimilaritySeedID(expr); ok {
		scores, err := tags.OverlapPercentsAgainst(s.db(), seedID, ids)
		if err != nil {
			logx.Debugf("gallery similarity scores: %v", err)
		}
		data.SimilarityPercent = scores
	}
	if inboxClustersActive(sortStr, orderStr, expr) {
		data.InboxClusterAtIdx = computeInboxClusters(result.Results, queryStr, page > 1, page < totalPages)
	}
	if inboxFilterActive(expr) {
		data.InboxUploadActive = true
		data.AcceptFileTypes = gallery.SupportedMIMETypes
	}
	data.HiddenByCeiling = hiddenByCeiling
	// The fragment uses it too, for its collapsed sidebar placeholder.
	data.SidebarURL = buildSidebarURL(queryStr, sortStr, orderStr, pageStr, q.Get("seed"), ids)

	if htmxGridTarget {
		// The sort select and status bar sit outside the swap target, so
		// only the fragment sends them out of band.
		data.SortSelectOOB = true
		data.StatusOOB = true
		s.renderTemplate(w, "partials/gallery_htmx.html", data)
		return
	}
	// The batch-strip dialog needs source labels the lazy sidebar won't bring.
	if cx := s.active(); cx != nil {
		data.SourceLabelCounts, _ = cx.SourceLabelCounts()
	}
	s.renderTemplate(w, "gallery.html", data)
}

type sidebarBundle struct {
	Tags         []models.Tag
	Folders      []gallery.FolderNode
	SourceLabels []gallery.SourceLabelCount
	Saved        []models.SavedSearch
}

func (s *Server) loadSavedSearches(logLabel string) []models.SavedSearch {
	out, err := db.QueryAll(s.db().Read, func(rows *sql.Rows) (models.SavedSearch, error) {
		var ss models.SavedSearch
		err := rows.Scan(&ss.ID, &ss.Name, &ss.Query, &ss.Sort, &ss.Order, &ss.Seed)
		return ss, err
	}, `SELECT id, name, query, sort, sort_order, seed FROM saved_searches ORDER BY name`)
	if err != nil {
		logx.Warnf("%s saved searches: %v", logLabel, err)
	}
	return out
}

// Only the reads that always hit the DB fan out: a goroutine per cached
// cx read takes a read-pool slot and costs more than it saves.
func (s *Server) sidebarLoad(pageImageIDs []int64, ceiling *Ceiling) sidebarBundle {
	var sb sidebarBundle
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sb.Tags, _ = search.SidebarTagsWithGlobalCount(s.db(), pageImageIDs)
	}()
	go func() {
		defer wg.Done()
		sb.Saved = s.loadSavedSearches("sidebar")
	}()
	if cx := s.active(); cx != nil {
		sb.Folders, _ = cx.FolderTreeUnder(ceiling)
		sb.SourceLabels, _ = cx.SourceLabelCountsUnder(ceiling)
	}
	wg.Wait()
	return sb
}

func (s *Server) gallerySidebar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	queryStr := q.Get("q")
	ceiling := resolveCeiling(r, s.active())

	// Empty ids is an empty page; only a missing ids param re-runs the search.
	var ids []int64
	if q.Has("ids") {
		if raw := q.Get("ids"); raw != "" {
			ids = make([]int64, 0, strings.Count(raw, ",")+1)
			for _, s := range strings.Split(raw, ",") {
				if id, err := strconv.ParseInt(s, 10, 64); err == nil {
					ids = append(ids, id)
				}
			}
		}
	} else {
		sortStr := q.Get("sort")
		sortStr = cmp.Or(sortStr, "newest")
		orderStr := q.Get("order")
		orderStr = cmp.Or(orderStr, search.DefaultOrder(sortStr))
		page := 1
		if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
			page = p
		}
		var randomSeed int64
		if sortStr == "random" {
			if seed, err := strconv.ParseInt(q.Get("seed"), 10, 64); err == nil {
				randomSeed = seed
			}
		}
		expr := search.Parse(queryStr)
		expr = ceiling.Apply(expr)
		sq := search.Query{
			Expr:       expr,
			Sort:       sortStr,
			Order:      orderStr,
			RandomSeed: randomSeed,
			Page:       page,
			Limit:      s.pageSize(r),
			SkipCount:  true,
		}
		result, err := search.Execute(s.db(), sq)
		if err != nil {
			logx.Errorf("sidebar search: %v", err)
			http.Error(w, "search error", http.StatusInternalServerError)
			return
		}
		ids = make([]int64, 0, len(result.Results))
		for _, img := range result.Results {
			ids = append(ids, img.ID)
		}
	}

	sb := s.sidebarLoad(ids, ceiling)

	s.renderTemplate(w, "partials/sidebar_content.html", map[string]any{
		"Query":             queryStr,
		"CSRFToken":         s.csrfToken(sessionFromContext(r.Context())),
		"SidebarTags":       sb.Tags,
		"FolderTree":        sb.Folders,
		"SourceLabelCounts": sb.SourceLabels,
		"SavedSearches":     sb.Saved,
		"ActiveTagTerms":    computeActiveTagTerms(queryStr),
	})
}

func (s *Server) sidebarBrowse(w http.ResponseWriter, r *http.Request) {
	queryStr := r.URL.Query().Get("q")
	ceiling := resolveCeiling(r, s.active())

	var (
		folders      []gallery.FolderNode
		sourceLabels []gallery.SourceLabelCount
		saved        []models.SavedSearch
	)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		saved = s.loadSavedSearches("sidebar-browse")
	}()
	if cx := s.active(); cx != nil {
		folders, _ = cx.FolderTreeUnder(ceiling)
		sourceLabels, _ = cx.SourceLabelCountsUnder(ceiling)
	}
	wg.Wait()

	s.renderTemplate(w, "partials/sidebar_browse.html", map[string]any{
		"Query":             queryStr,
		"CSRFToken":         s.csrfToken(sessionFromContext(r.Context())),
		"FolderTree":        folders,
		"SourceLabelCounts": sourceLabels,
		"SavedSearches":     saved,
	})
}

type PageSizeChoice struct {
	Value    string
	Label    string
	Title    string
	Selected bool
}

// The Default button is the only way back to a configured size the ramp lacks.
func (s *Server) pageSizeChoices(r *http.Request) []PageSizeChoice {
	s.cfgMu.RLock()
	configured := s.cfg.UI.PageSize
	s.cfgMu.RUnlock()

	override := pageSizeOverride(r)
	out := make([]PageSizeChoice, 0, len(PageSizeOptions)+1)
	out = append(out, PageSizeChoice{
		Value:    "default",
		Label:    "Def",
		Title:    fmt.Sprintf("Default (%d)", configured),
		Selected: override == 0,
	})
	for _, n := range PageSizeOptions {
		label := strconv.Itoa(n)
		out = append(out, PageSizeChoice{Value: label, Label: label, Selected: override == n})
	}
	return out
}

func buildSidebarURL(q, sort, order, page, seed string, ids []int64) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if sort != "" {
		v.Set("sort", sort)
	}
	if order != "" {
		v.Set("order", order)
	}
	if page != "" {
		v.Set("page", page)
	}
	if seed != "" {
		v.Set("seed", seed)
	}
	var sb strings.Builder
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatInt(id, 10))
	}
	v.Set("ids", sb.String())
	return "/internal/sidebar?" + v.Encode()
}

// NOT, OR and wildcard leaves stay out: the toggle can't remove them cleanly.
func computeActiveTagTerms(query string) map[string]bool {
	set := make(map[string]bool)
	expr := search.Parse(query)
	if expr == nil {
		return set
	}
	search.WalkAndedLeaves(expr, func(e search.Expr) {
		switch v := e.(type) {
		case search.TagExpr:
			if v.Tag != "" && v.Wildcard == "" {
				set[v.Tag] = true
			}
		case search.FilterExpr:
			set[v.Key+":"+strings.ToLower(v.Val)] = true
		}
	})
	return set
}

func inboxFilterActive(expr search.Expr) bool {
	if expr == nil {
		return false
	}
	var found bool
	search.WalkAndedLeaves(expr, func(e search.Expr) {
		if v, ok := e.(search.FilterExpr); ok && v.Key == "inbox" && strings.ToLower(v.Val) == "true" {
			found = true
		}
	})
	return found
}

func collectionFilterActive(expr search.Expr) bool {
	if expr == nil {
		return false
	}
	var found bool
	search.WalkAndedLeaves(expr, func(e search.Expr) {
		if v, ok := e.(search.FilterExpr); ok && v.Key == "collection" && v.Val != "" {
			found = true
		}
	})
	return found
}

func searchWarning(expr search.Expr) string {
	unknown := unknownFilterValues(expr)
	if len(unknown) == 0 {
		return ""
	}
	return "Unknown filter value: " + strings.Join(unknown, ", ")
}

func unknownFilterValues(expr search.Expr) []string {
	if expr == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	search.WalkLeaves(expr, func(e search.Expr) bool {
		if v, ok := e.(search.FilterExpr); ok && !searchkw.ValueKnown(v.Key, v.Val) {
			token := v.Key + ":" + v.Val
			if !seen[token] {
				seen[token] = true
				out = append(out, token)
			}
		}
		return true
	})
	return out
}

// Clusters need newest-first rows; the upload zone shows under any sort.
func inboxClustersActive(sort, order string, expr search.Expr) bool {
	if sort != "newest" || order != "desc" {
		return false
	}
	return inboxFilterActive(expr)
}

func computeInboxClusters(images []models.Image, queryStr string, moreAbove, moreBelow bool) []*inboxCluster {
	if len(images) == 0 {
		return nil
	}
	markers := make([]*inboxCluster, len(images))
	gap := time.Duration(batchGapMinutes) * time.Minute
	start := 0
	for i := 1; i <= len(images); i++ {
		closeCluster := i == len(images)
		if !closeCluster {
			prevB, nextB := images[i-1].UploadBatch, images[i].UploadBatch
			switch {
			case prevB != nil || nextB != nil:
				closeCluster = !sameBatch(prevB, nextB)
			default:
				// >= so a 15-minute schedule gets one cluster per run.
				closeCluster = images[i-1].IngestedAt.Sub(images[i].IngestedAt) >= gap
			}
		}
		if closeCluster {
			spills := (start == 0 && moreAbove) || (i == len(images) && moreBelow)
			markers[start] = buildInboxCluster(images[start:i], queryStr, !spills)
			start = i
		}
	}
	return markers
}

func sameBatch(a, b *int64) bool { return a != nil && b != nil && *a == *b }

func buildInboxCluster(rows []models.Image, queryStr string, whole bool) *inboxCluster {
	// Local time: the date: filter reads its bounds in the local zone.
	newest := rows[0].IngestedAt.In(time.Local)
	oldest := rows[len(rows)-1].IngestedAt.In(time.Local)
	dateLabel := newest.Format("2006-01-02")
	rangeLabel := oldest.Format("15:04")
	if len(rows) > 1 && !oldest.Equal(newest) {
		rangeLabel = oldest.Format("15:04") + " -> " + newest.Format("15:04")
	}
	// Minute-precise date bounds would take other uploads from that minute too.
	leaf := " date:" + oldest.Format("2006-01-02T15:04") + ".." + newest.Format("2006-01-02T15:04")
	if rows[0].UploadBatch != nil {
		leaf = " batch:" + strconv.FormatInt(*rows[0].UploadBatch, 10)
	}
	clusterQ := "inbox:true" + leaf
	if queryStr != "" && queryStr != "inbox:true" {
		clusterQ = queryStr + leaf
	}
	return &inboxCluster{
		Count:      len(rows),
		DateLabel:  dateLabel,
		RangeLabel: rangeLabel,
		RangeLink:  "/?" + url.Values{"q": []string{clusterQ}}.Encode(),
		RangeQuery: clusterQ,
		Whole:      whole,
	}
}
