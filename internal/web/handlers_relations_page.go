package web

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/relations"
)

const maxPhashDistance = 12

func (s *Server) findPairsDistance() int {
	s.cfgMu.Lock()
	d := s.cfg.Relations.DefaultDistance
	s.cfgMu.Unlock()
	if d < 0 {
		return 0
	}
	if d > maxPhashDistance {
		return maxPhashDistance
	}
	return d
}

func (s *Server) tagPairsEnabled() bool {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.cfg.Relations.TagPairs
}

func applyRelationsConfig(rc config.RelationsConfig) {
	d := rc.DefaultDistance
	if d < 0 || d > maxPhashDistance {
		d = 4
	}
	relations.IncrementalProbeDistance.Store(int32(d))
	relations.IncrementalProbeEnabled.Store(rc.IncrementalOnIngest)
}

// The on-ingest probe has no switch: a save always turns it on.
func (s *Server) settingsRelationsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	d, err := strconv.Atoi(r.FormValue("default_distance"))
	if err != nil || d < 0 || d > maxPhashDistance {
		flashStatus(w, http.StatusBadRequest, "Distance must be an integer 0..12.")
		return
	}
	order := r.FormValue("default_session_order")
	if !validOrderModes[order] {
		flashStatus(w, http.StatusBadRequest, "Unknown session order.")
		return
	}
	threshold := config.DefaultTagPairThreshold
	if raw := r.FormValue("tag_pair_threshold"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			flashStatus(w, http.StatusBadRequest, "Match strength must be a number between 70 and 100.")
			return
		}
		threshold = config.ClampTagPairThreshold(v / 100)
	}
	tagPairs := r.FormValue("tag_pairs") == "on"
	s.cfgMu.Lock()
	before := s.cfg.Relations
	s.cfg.Relations.DefaultDistance = d
	s.cfg.Relations.DefaultSessionOrder = order
	s.cfg.Relations.IncrementalOnIngest = true
	s.cfg.Relations.TagPairs = tagPairs
	s.cfg.Relations.TagPairThreshold = threshold
	rc := s.cfg.Relations
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		flashStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	applyRelationsConfig(rc)
	pruned := ""
	if before.DefaultDistance != d || before.TagPairs != tagPairs || before.TagPairThreshold != threshold {
		if n := s.pruneRelationQueues(r.Context(), rc); n > 0 {
			pruned = fmt.Sprintf(" %d stale pair(s) dropped.", n)
		}
	}
	logx.Infof("settings: relations { distance=%d order=%s tag_pairs=%v threshold=%.2f }", d, order, tagPairs, threshold)
	_, _ = fmt.Fprintf(w,
		`<div class="flash flash-ok">Saved. distance=%d order=%s tag pairs=%v (%.0f%%)%s</div>`,
		d, order, tagPairs, threshold*100, pruned)
}

// Every gallery, not only the active one: the settings are app-wide but
// each gallery keeps its own queue.
func (s *Server) pruneRelationQueues(ctx context.Context, rc config.RelationsConfig) int {
	ctxs := s.allContexts()
	opts := relations.FindPairsOptions{
		Distance:         rc.DefaultDistance,
		TagPairs:         rc.TagPairs,
		TagPairThreshold: config.ClampTagPairThreshold(rc.TagPairThreshold),
	}
	total := 0
	for _, cx := range ctxs {
		if cx.DB == nil {
			continue
		}
		n, err := relations.PruneQueue(ctx, cx.DB, opts)
		if err != nil {
			logx.Warnf("prune pair queue %q: %v", cx.Name, err)
			continue
		}
		total += n
	}
	return total
}

type relationsCounts struct {
	PhashMissing    int
	QueueOpen       int
	QueueSkipped    int
	QueueBySource   []queueSourceCount
	DupGroups       int
	AltGroups       int
	VersionChains   int
	DerivativeTrees int
	NotRelatedPairs int
}

type queueSourceCount struct {
	Label string
	Count int
}

var queueSourceLabels = []struct{ source, label string }{
	{relations.SourcePhash, "image similarity"},
	{relations.SourceTags, "rare tag similarity"},
	{relations.SourceBoth, "both"},
	{relations.SourceReview, "reopened"},
}

func queueCounts(cx *galleryCtx, ceiling *Ceiling) (open, skipped int, bySource []queueSourceCount) {
	var rank *int
	if r, active := ceiling.RankCeiling(); active {
		rank = &r
	}
	open, skipped, counts, err := cx.RelationsSvc.QueueBySource(rank)
	if err != nil {
		logx.Debugf("relations queue counts: %v", err)
		return 0, 0, nil
	}
	if len(counts) < 2 {
		return open, skipped, nil
	}
	bySource = make([]queueSourceCount, 0, len(counts))
	for _, s := range queueSourceLabels {
		if n := counts[s.source]; n > 0 {
			bySource = append(bySource, queueSourceCount{Label: s.label, Count: n})
		}
	}
	return open, skipped, bySource
}

type browseCard struct {
	Kind             string
	GroupID          int64
	Members          []int64
	Original         int64
	CreatedAt        string
	MemberIngestedAt map[int64]string
	Graph            *derivGraph
	Roots            []int64
}

func (s *Server) relationsPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	ceiling := resolveCeiling(r, cx)
	counts := loadRelationsCounts(cx, ceiling, "")
	s.renderTemplate(w, "relations.html", relationsPageData{
		baseData:      s.base(r, "relations", "Relations - "+s.booruName()),
		Counts:        counts,
		ActiveGallery: s.activeGallery(),
	})
}

type relationsPageData struct {
	baseData
	Counts        relationsCounts
	ActiveGallery string
}

func (s *Server) browseGroupsRedirect(w http.ResponseWriter, r *http.Request) {
	target := "/relations/browse?kind=duplicate"
	switch r.URL.Query().Get("kind") {
	case "alt":
		target = "/relations/browse?kind=alternate"
	case "version":
		target = "/relations/browse?kind=version"
	case "derivative":
		target = "/relations/browse?kind=derivative"
	case "dup", "":
		target = "/relations/browse?kind=duplicate"
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// The skipKind count (version or derivative) is left at zero for the
// caller to fill from its own card walk.
func loadRelationsCounts(cx *galleryCtx, ceiling *Ceiling, skipKind string) relationsCounts {
	var c relationsCounts
	get := func(q string, dst *int, args ...any) {
		if err := cx.DB.Read.QueryRow(q, args...).Scan(dst); err != nil {
			logx.Debugf("relations counts %q: %v", q, err)
		}
	}
	if n, err := cx.PhashMissingUnder(ceiling); err == nil {
		c.PhashMissing = n
	}
	c.QueueOpen, c.QueueSkipped, c.QueueBySource = queueCounts(cx, ceiling)
	if where, args := ceiling.WhereGroupClean("dup_group_members", "dup_groups.id"); where != "" {
		get(`SELECT COUNT(*) FROM dup_groups WHERE `+where, &c.DupGroups, args...)
	} else {
		get(`SELECT COUNT(*) FROM dup_groups`, &c.DupGroups)
	}
	if where, args := ceiling.WhereGroupClean("alt_group_members", "alt_groups.id"); where != "" {
		get(`SELECT COUNT(*) FROM alt_groups WHERE `+where, &c.AltGroups, args...)
	} else {
		get(`SELECT COUNT(*) FROM alt_groups`, &c.AltGroups)
	}
	if skipKind != "version" {
		if _, total, err := loadVersionChainCards(cx, 0, ceiling); err == nil {
			c.VersionChains = total
		} else {
			logx.Debugf("relations counts version chains: %v", err)
		}
	}
	if skipKind != "derivative" {
		if _, total, err := loadDerivativeTreeCards(cx, 0, ceiling); err == nil {
			c.DerivativeTrees = total
		} else {
			logx.Debugf("relations counts derivative trees: %v", err)
		}
	}
	if where, args := ceiling.WhereTwo("a_image_id", "b_image_id"); where != "" {
		get(`SELECT COUNT(*) FROM not_related_pairs WHERE `+where, &c.NotRelatedPairs, args...)
	} else {
		get(`SELECT COUNT(*) FROM not_related_pairs`, &c.NotRelatedPairs)
	}
	return c
}

// The int is the version or derivative total past the ceiling and before
// the limit; 0 for the other kinds.
func loadBrowseCardsByKind(cx *galleryCtx, kind, sort string, limit, offset int, ceiling *Ceiling) ([]browseCard, int, error) {
	groupCardsWhere := func(membersTable, groupCol string) (string, []any) {
		w, a := ceiling.WhereGroupClean(membersTable, groupCol)
		if w == "" {
			return "", nil
		}
		return " AND " + w, a
	}
	var cards []browseCard
	var walkedTotal int
	switch kind {
	case "duplicate", "alternate":
		g := browseGroupKinds[kind]
		where, args := groupCardsWhere(g.membersTable, g.groupTable+".id")
		rows, err := cx.DB.Read.Query(
			`SELECT `+g.cols+` FROM `+g.groupTable+` WHERE 1=1`+where+` `+g.sortClause(sort)+` LIMIT ? OFFSET ?`,
			append(args, limit, offset)...,
		)
		if err != nil {
			return nil, 0, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, original int64
			var createdAt string
			if err := rows.Scan(&id, &original, &createdAt); err != nil {
				return nil, 0, err
			}
			members, mErr := scanGroupMembers(cx, g.membersTable, id)
			if mErr != nil {
				return nil, 0, mErr
			}
			cards = append(cards, browseCard{Kind: kind, GroupID: id, Members: members, Original: original, CreatedAt: humanISOTime(createdAt)})
		}
		if err := rows.Err(); err != nil {
			return nil, 0, err
		}
	case "version":
		// Annotated before the sort: newest_member reads the ingest dates.
		chains, total, cErr := loadVersionChainCards(cx, 0, ceiling)
		if cErr != nil {
			return nil, 0, cErr
		}
		if sort == "newest_member" {
			if aErr := annotateBrowseCardIngestedAt(cx, chains); aErr != nil {
				logx.Warnf("browse cards ingest-dates version: %v", aErr)
			}
		}
		sortVersionCards(chains, sort)
		cards = append(cards, sliceWindow(chains, offset, limit)...)
		walkedTotal = total
	case "derivative":
		trees, total, tErr := loadDerivativeTreeCards(cx, 0, ceiling)
		if tErr != nil {
			return nil, 0, tErr
		}
		sortDerivativeCards(trees, sort)
		cards = append(cards, sliceWindow(trees, offset, limit)...)
		walkedTotal = total
	case "not_related":
		where, args := ceiling.WhereTwo("a_image_id", "b_image_id")
		q := `SELECT a_image_id, b_image_id, created_at FROM not_related_pairs`
		if where != "" {
			q += ` WHERE ` + where
		}
		q += ` ORDER BY rowid DESC LIMIT ? OFFSET ?`
		nrRows, err := cx.DB.Read.Query(q, append(args, limit, offset)...)
		if err != nil {
			return nil, 0, err
		}
		defer func() { _ = nrRows.Close() }()
		for nrRows.Next() {
			var a, b int64
			var createdAt string
			if err := nrRows.Scan(&a, &b, &createdAt); err != nil {
				return nil, 0, err
			}
			cards = append(cards, browseCard{Kind: "not_related", Members: []int64{a, b}, CreatedAt: humanISOTime(createdAt)})
		}
		if err := nrRows.Err(); err != nil {
			return nil, 0, err
		}
	default:
		return nil, 0, nil
	}
	if err := annotateBrowseCardIngestedAt(cx, cards); err != nil {
		logx.Warnf("browse cards ingest-dates %s: %v", kind, err)
	}
	return cards, walkedTotal, nil
}

var browseGroupKinds = map[string]struct {
	groupTable   string
	membersTable string
	cols         string
	sortClause   func(string) string
}{
	"duplicate": {"dup_groups", "dup_group_members", "dup_groups.id, dup_groups.original_image_id, dup_groups.created_at", dupSortClause},
	"alternate": {"alt_groups", "alt_group_members", "alt_groups.id, 0, alt_groups.created_at", altSortClause},
}

func dupSortClause(sort string) string {
	switch sort {
	case "size":
		return `ORDER BY (SELECT COUNT(*) FROM dup_group_members WHERE group_id = dup_groups.id) DESC, dup_groups.id DESC`
	case "original_added":
		return `ORDER BY (SELECT ingested_at FROM images WHERE id = dup_groups.original_image_id) DESC, dup_groups.id DESC`
	}
	return `ORDER BY dup_groups.id DESC`
}

func altSortClause(sort string) string {
	if sort == "size" {
		return `ORDER BY (SELECT COUNT(*) FROM alt_group_members WHERE group_id = alt_groups.id) DESC, alt_groups.id DESC`
	}
	return `ORDER BY alt_groups.id DESC`
}

func sortVersionCards(cards []browseCard, sortKey string) {
	switch sortKey {
	case "length":
		sort.SliceStable(cards, func(i, j int) bool {
			return len(cards[i].Members) > len(cards[j].Members)
		})
	case "newest_member":
		newest := func(c browseCard) string {
			var best string
			for _, m := range c.Members {
				if d, ok := c.MemberIngestedAt[m]; ok && d > best {
					best = d
				}
			}
			return best
		}
		sort.SliceStable(cards, func(i, j int) bool {
			return newest(cards[i]) > newest(cards[j])
		})
	}
}

func sortDerivativeCards(cards []browseCard, sortKey string) {
	if sortKey == "size" {
		sort.SliceStable(cards, func(i, j int) bool {
			return len(cards[i].Members) > len(cards[j].Members)
		})
	}
}

func sliceWindow(cards []browseCard, offset, limit int) []browseCard {
	if offset >= len(cards) {
		return nil
	}
	end := offset + limit
	if limit <= 0 || end > len(cards) {
		end = len(cards)
	}
	return cards[offset:end]
}

func annotateBrowseCardIngestedAt(cx *galleryCtx, cards []browseCard) error {
	if len(cards) == 0 {
		return nil
	}
	idSet := map[int64]struct{}{}
	for _, c := range cards {
		for _, m := range c.Members {
			idSet[m] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return nil
	}
	placeholders, args := db.InPlaceholders(slices.Collect(maps.Keys(idSet)))
	rows, err := cx.DB.Read.Query(
		`SELECT id, ingested_at FROM images WHERE id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	dates := map[int64]string{}
	for rows.Next() {
		var id int64
		var ingested string
		if scanErr := rows.Scan(&id, &ingested); scanErr != nil {
			return scanErr
		}
		dates[id] = humanISOTime(ingested)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range cards {
		out := make(map[int64]string, len(cards[i].Members))
		for _, m := range cards[i].Members {
			if d, ok := dates[m]; ok {
				out[m] = d
			}
		}
		cards[i].MemberIngestedAt = out
	}
	return nil
}

func humanISOTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.In(time.Local).Format("2006-01-02 15:04:05")
}

func humanISODate(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		if i := strings.IndexByte(s, 'T'); i > 0 {
			return s[:i]
		}
		return s
	}
	return t.In(time.Local).Format("2006-01-02")
}

func sortedRootsDesc(rootSet map[int64]bool) []int64 {
	roots := slices.Sorted(maps.Keys(rootSet))
	slices.Reverse(roots)
	return roots
}

func finalizeCards(cards []browseCard, limit int) ([]browseCard, int) {
	total := len(cards)
	sort.SliceStable(cards, func(i, j int) bool {
		return cards[i].CreatedAt > cards[j].CreatedAt
	})
	if limit > 0 && len(cards) > limit {
		cards = cards[:limit]
	}
	return cards, total
}

// A chain with any member above the ceiling is dropped whole, so a hidden
// image's relations never show.
func loadVersionChainCards(cx *galleryCtx, limit int, ceiling *Ceiling) ([]browseCard, int, error) {
	rows, err := cx.DB.Read.Query(`SELECT child_image_id, parent_image_id, created_at FROM version_edges`)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	type edgeMeta struct {
		parent    int64
		createdAt string
	}
	edges := map[int64]edgeMeta{}
	childOf := map[int64]int64{} // parent -> child (UNIQUE per schema)
	for rows.Next() {
		var c, p int64
		var ts string
		if scanErr := rows.Scan(&c, &p, &ts); scanErr != nil {
			return nil, 0, scanErr
		}
		edges[c] = edgeMeta{parent: p, createdAt: ts}
		childOf[p] = c
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	rootSet := map[int64]bool{}
	for _, em := range edges {
		if _, hasParent := edges[em.parent]; !hasParent {
			rootSet[em.parent] = true
		}
	}
	roots := sortedRootsDesc(rootSet)
	cards := make([]browseCard, 0, len(roots))
	for _, root := range roots {
		members := []int64{root}
		latestTS := ""
		cur := root
		for {
			next, ok := childOf[cur]
			if !ok {
				break
			}
			members = append(members, next)
			if em, ok := edges[next]; ok && em.createdAt > latestTS {
				latestTS = em.createdAt
			}
			cur = next
		}
		if ceiling.AnyTainted(members) {
			continue
		}
		cards = append(cards, browseCard{
			Kind:      "version",
			Members:   members,
			CreatedAt: humanISOTime(latestTS),
		})
	}
	cards, total := finalizeCards(cards, limit)
	return cards, total, nil
}

// One card per component, which has several roots when an image was made
// from several sources.
func loadDerivativeTreeCards(cx *galleryCtx, limit int, ceiling *Ceiling) ([]browseCard, int, error) {
	rows, err := cx.DB.Read.Query(`SELECT derivative_image_id, source_image_id, created_at FROM derivative_edges`)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	derivativesOf := map[int64][]int64{}
	sourcesOf := map[int64][]int64{}
	derivCreated := map[int64]string{}
	for rows.Next() {
		var d, src int64
		var ts string
		if scanErr := rows.Scan(&d, &src, &ts); scanErr != nil {
			return nil, 0, scanErr
		}
		derivativesOf[src] = append(derivativesOf[src], d)
		sourcesOf[d] = append(sourcesOf[d], src)
		if ts > derivCreated[d] {
			derivCreated[d] = ts
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for src := range derivativesOf {
		sort.Slice(derivativesOf[src], func(i, j int) bool {
			return derivativesOf[src][i] < derivativesOf[src][j]
		})
	}
	rootSet := map[int64]bool{}
	for src := range derivativesOf {
		if len(sourcesOf[src]) == 0 {
			rootSet[src] = true
		}
	}
	emitted := map[int64]bool{}
	cards := make([]browseCard, 0, len(rootSet))
	for _, root := range sortedRootsDesc(rootSet) {
		if emitted[root] {
			continue
		}
		compRoots := componentRoots(root, derivativesOf, sourcesOf)
		members := componentMembers(compRoots, derivativesOf, sourcesOf)
		for _, m := range members {
			emitted[m] = true
		}
		if ceiling.AnyTainted(members) {
			continue
		}
		latestTS := ""
		for _, m := range members {
			if ts := derivCreated[m]; ts > latestTS {
				latestTS = ts
			}
		}
		cards = append(cards, browseCard{
			Kind:      "derivative",
			Members:   members,
			Graph:     layOutDerivatives(members, sourcesOf),
			Roots:     compRoots,
			CreatedAt: humanISOTime(latestTS),
		})
	}
	cards, total := finalizeCards(cards, limit)
	return cards, total, nil
}

// X coordinates are per-mille of the graph's width, not pixels, so the
// wires follow the CSS columns at any card width.
const (
	derivSpanX = 1000
	// Must match .deriv-band's height in main.css.
	derivBandH = 44
)

type derivNode struct {
	ID      int64
	Row     int
	Col     int
	Sources []int64
}

type derivSeg struct {
	X1, X2   int
	From, To int64
}

type derivGraph struct {
	Rows  [][]derivNode
	Bands [][]derivSeg
	Cols  int
	Width int
	BandH int
}

func layOutDerivatives(members []int64, sourcesOf map[int64][]int64) *derivGraph {
	if len(members) == 0 {
		return nil
	}
	row := make(map[int64]int, len(members))
	for _, id := range members {
		row[id] = 0
	}
	// Repeated until nothing moves: members are not in dependency order.
	for pass := 0; pass <= relations.MaxVersionChainDepth; pass++ {
		moved := false
		for _, id := range members {
			want := 0
			for _, src := range sourcesOf[id] {
				if r, ok := row[src]; ok && r+1 > want {
					want = r + 1
				}
			}
			if want > row[id] {
				row[id] = want
				moved = true
			}
		}
		if !moved {
			break
		}
	}

	byRow := map[int][]int64{}
	depth := 0
	for _, id := range members {
		byRow[row[id]] = append(byRow[row[id]], id)
		if row[id] > depth {
			depth = row[id]
		}
	}
	g := &derivGraph{Rows: make([][]derivNode, depth+1), Bands: make([][]derivSeg, depth), BandH: derivBandH}
	col := make(map[int64]int, len(members))
	widest := 0
	// Top down, so every source already has its column when parentMean
	// reads it.
	for r := 0; r <= depth; r++ {
		ids := byRow[r]
		slices.Sort(ids)
		if r > 0 {
			slices.SortStableFunc(ids, func(a, b int64) int {
				return cmp.Compare(parentMean(a, sourcesOf, col), parentMean(b, sourcesOf, col))
			})
		}
		g.Rows[r] = make([]derivNode, len(ids))
		for c, id := range ids {
			col[id] = c
			g.Rows[r][c] = derivNode{ID: id, Row: r, Col: c, Sources: sourcesOf[id]}
		}
		if len(ids) > widest {
			widest = len(ids)
		}
	}
	g.Cols, g.Width = widest, derivSpanX

	// Counted in half-columns: a shorter row is centred under the widest,
	// as the CSS does, which can leave half a column over.
	centreX := func(id int64) int {
		halves := widest - len(g.Rows[row[id]]) + 2*col[id] + 1
		return halves * derivSpanX / (2 * widest)
	}
	for _, id := range members {
		for _, src := range sourcesOf[id] {
			from, to := row[src], row[id]
			if to <= from {
				continue
			}
			x1, x2, span := centreX(src), centreX(id), to-from
			for b := from; b < to; b++ {
				g.Bands[b] = append(g.Bands[b], derivSeg{
					X1:   x1 + (x2-x1)*(b-from)/span,
					X2:   x1 + (x2-x1)*(b+1-from)/span,
					From: src,
					To:   id,
				})
			}
		}
	}
	return g
}

func parentMean(id int64, sourcesOf map[int64][]int64, col map[int64]int) float64 {
	sum, n := 0, 0
	for _, src := range sourcesOf[id] {
		if c, ok := col[src]; ok {
			sum += c
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(sum) / float64(n)
}

func componentMembers(roots []int64, derivativesOf, sourcesOf map[int64][]int64) []int64 {
	seen := map[int64]bool{}
	var queue []int64
	for _, r := range roots {
		if !seen[r] {
			seen[r] = true
			queue = append(queue, r)
		}
	}
	for i := 0; i < len(queue); i++ {
		for _, m := range slices.Concat(derivativesOf[queue[i]], sourcesOf[queue[i]]) {
			if !seen[m] {
				seen[m] = true
				queue = append(queue, m)
			}
		}
	}
	slices.Sort(queue)
	return queue
}

func componentRoots(start int64, derivativesOf, sourcesOf map[int64][]int64) []int64 {
	seen := map[int64]bool{start: true}
	queue := []int64{start}
	var roots []int64
	for i := 0; i < len(queue); i++ {
		n := queue[i]
		if len(sourcesOf[n]) == 0 {
			roots = append(roots, n)
		}
		for _, m := range slices.Concat(derivativesOf[n], sourcesOf[n]) {
			if !seen[m] {
				seen[m] = true
				queue = append(queue, m)
			}
		}
	}
	slices.Sort(roots)
	return roots
}

func scanGroupMembers(cx *galleryCtx, table string, groupID int64) ([]int64, error) {
	return db.QueryIDs(cx.DB.Read, `SELECT image_id FROM `+table+` WHERE group_id = ? ORDER BY image_id`, groupID)
}

var validBrowseKinds = map[string]bool{
	"duplicate":   true,
	"alternate":   true,
	"version":     true,
	"derivative":  true,
	"not_related": true,
}

// The first entry is each kind's default.
var browseSortsByKind = map[string][]string{
	"duplicate":   {"recent", "size", "original_added"},
	"alternate":   {"recent", "size"},
	"version":     {"recent", "length", "newest_member"},
	"derivative":  {"recent", "size"},
	"not_related": {"recent"},
}

func resolveBrowseSort(kind, requested string) string {
	allowed := browseSortsByKind[kind]
	if len(allowed) == 0 {
		return "recent"
	}
	for _, s := range allowed {
		if s == requested {
			return s
		}
	}
	return allowed[0]
}

const browseRelationsPageSize = 60

func (s *Server) browseRelationsPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	kind = cmp.Or(kind, "duplicate")
	if !validBrowseKinds[kind] {
		s.notFoundHandler(w, r)
		return
	}
	page := 1
	requested := 0
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil {
		requested, page = p, p
	}
	if page < 1 {
		page = 1
	}
	// Past this the offset wraps negative and slices out of range.
	page = min(page, math.MaxInt/browseRelationsPageSize)
	sort := resolveBrowseSort(kind, r.URL.Query().Get("sort"))
	ceiling := resolveCeiling(r, cx)
	// A version or derivative count comes from the card walk, so the cards
	// load before the page clamp and an out-of-range htmx page loads twice.
	counts := loadRelationsCounts(cx, ceiling, kind)
	offset := (page - 1) * browseRelationsPageSize
	cards, walkedTotal, err := loadBrowseCardsByKind(cx, kind, sort, browseRelationsPageSize, offset, ceiling)
	if err != nil {
		logx.Warnf("browse cards %s: %v", kind, err)
		http.Error(w, "load cards", http.StatusInternalServerError)
		return
	}
	switch kind {
	case "version":
		counts.VersionChains = walkedTotal
	case "derivative":
		counts.DerivativeTrees = walkedTotal
	}
	total := kindTotal(counts, kind)
	totalPages := 1
	if total > 0 {
		totalPages = (total + browseRelationsPageSize - 1) / browseRelationsPageSize
	}
	if page > totalPages {
		page = totalPages
	}
	if requested != 0 && requested != page {
		clampedQ := r.URL.Query()
		clampedQ.Set("page", strconv.Itoa(page))
		clampedURL := "/relations/browse?" + clampedQ.Encode()
		if isHTMXRequest(r) {
			w.Header().Set("HX-Push-Url", clampedURL)
		} else {
			http.Redirect(w, r, clampedURL, http.StatusSeeOther)
			return
		}
	}
	if clamped := (page - 1) * browseRelationsPageSize; clamped != offset {
		cards, _, err = loadBrowseCardsByKind(cx, kind, sort, browseRelationsPageSize, clamped, ceiling)
		if err != nil {
			logx.Warnf("browse cards %s (clamped): %v", kind, err)
			http.Error(w, "load cards", http.StatusInternalServerError)
			return
		}
	}
	s.renderTemplate(w, "relations_browse.html", browseRelationsData{
		baseData:      s.base(r, "relations", "Browse relations - "+s.booruName()),
		ActiveGallery: s.activeGallery(),
		Kind:          kind,
		Cards:         cards,
		Counts:        counts,
		Page:          page,
		TotalPages:    totalPages,
		Sort:          sort,
		SortOptions:   browseSortsByKind[kind],
	})
}

func kindTotal(c relationsCounts, kind string) int {
	switch kind {
	case "duplicate":
		return c.DupGroups
	case "alternate":
		return c.AltGroups
	case "version":
		return c.VersionChains
	case "derivative":
		return c.DerivativeTrees
	case "not_related":
		return c.NotRelatedPairs
	}
	return 0
}

type browseRelationsData struct {
	baseData
	ActiveGallery string
	Kind          string
	Cards         []browseCard
	Counts        relationsCounts
	Page          int
	TotalPages    int
	Sort          string
	SortOptions   []string
}
