package search

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

type Query struct {
	Expr        Expr
	Sort        string // "newest" | "filesize" | "random" | "order" | "similarity"
	Order       string // "asc" | "desc"
	RandomSeed  int64
	Page        int // 1-based
	Limit       int
	PresetTotal *int
	SkipCount   bool
	// Empty skips the adjacency cache.
	CacheKey        string
	OrderCollection string
}

func Execute(database *db.DB, q Query) (*models.SearchResult, error) {
	started := time.Now()
	page := max(q.Page, 1)
	limit := q.Limit
	if limit < 1 {
		limit = 40
	}
	// page comes unbounded from the URL; page*limit*driverIDBoundMargin
	// must not overflow into a negative offset or slice bound.
	page = min(page, math.MaxInt/limit/driverIDBoundMargin)

	if q.CacheKey != "" && !q.SkipCount && q.PresetTotal == nil {
		if ids, ok := AdjacencyCacheGet(q.CacheKey); ok {
			return executeFromCachedIDs(database, ids, page, limit)
		}
	}

	driverLegs, _ := pickAndDriverTag(database, q.Expr, q.Sort == "random")

	// ids mostly rise with ingested_at, so a dense intersection's first pages
	// sit in the newest page*limit*driverIDBoundMargin visible ids. asc wants
	// the other end, and missing: rows fall outside that walk.
	idBounded := false
	if len(driverLegs) >= 2 &&
		(q.Sort == "" || q.Sort == "newest") &&
		q.Order != "asc" &&
		!containsMissingFilter(q.Expr) {
		if total, ok := fastTagTotal(database, q.Expr); ok {
			if visible, vOk := fastVisibleCount(database); vOk &&
				total*driverIDBoundDensityCutoff >= visible {
				targetOffset := (page * limit) * driverIDBoundMargin
				var bound int64
				err := database.Read.QueryRow(
					`SELECT id FROM images INDEXED BY idx_images_ingested_visible
					 WHERE is_missing = 0
					 ORDER BY ingested_at DESC, id DESC
					 LIMIT 1 OFFSET ?`, targetOffset,
				).Scan(&bound)
				if err == nil {
					for i := range driverLegs {
						driverLegs[i].idBound = bound
					}
					idBounded = true
				}
				// ErrNoRows: the library is smaller than the offset, so
				// there is nothing to cut.
			}
		}
	}

	where, args, hasMissingFilter, ceilingRewrote := buildWhereDBDriverFull(q.Expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)

	where = andDefaultVisible(where, hasMissingFilter)

	orderClause, orderArgs, rankSeed, hasRankSeed := viewOrder(database, q.Expr, q.Sort, q.Order, q.RandomSeed, q.OrderCollection)

	// Similarity has no key column: COUNT and the scored ORDER BY would each
	// walk the whole match set for one page, so the fan runs first, on any
	// page. An empty or capped fan falls through rather than guess a total.
	if q.Sort == "similarity" && hasRankSeed && q.CacheKey != "" &&
		!q.SkipCount && q.PresetTotal == nil && AdjacencyCacheTryAcquireFan(q.CacheKey) {
		ctx, cancel := context.WithTimeout(context.Background(), fanBudget(time.Since(started)))
		ids := fanSimilarityIDs(ctx, database, rankSeed, q.Order, where, args)
		cancel()
		AdjacencyCacheReleaseFan(q.CacheKey)
		if len(ids) > 0 && len(ids) < adjacencyCacheMaxIDs {
			AdjacencyCacheSet(q.CacheKey, ids)
			return executeFromCachedIDs(database, ids, page, limit)
		}
		AdjacencyCacheMarkFanOverBudget(q.CacheKey)
	}

	offset := (page - 1) * limit

	var total int
	fastEmpty := false
	switch {
	case q.SkipCount:
	case q.PresetTotal != nil:
		total = *q.PresetTotal
	default:
		// The fast counts are visible-only, which a missing: filter
		// doesn't want.
		if !hasMissingFilter {
			if n, ok := fastTagTotal(database, q.Expr); ok {
				total = n
				fastEmpty = n == 0
				break
			}
		}
		countSQL := "SELECT COUNT(*) FROM images i WHERE " + where
		if err := database.Read.QueryRow(countSQL, args...).Scan(&total); err != nil {
			return nil, fmt.Errorf("count query: %w", err)
		}
	}

	if fastEmpty {
		return &models.SearchResult{Page: page, Limit: limit, Total: 0}, nil
	}

	indexHint := sortIndexHint(q.Expr, q.Sort, hasMissingFilter, ceilingRewrote)

	// The planner drives from a multi-leg INTERSECT anyway; the pin only turns
	// each probe into a skip-scan of the partial index instead of a rowid seek.
	// The id-only fan keeps it: read to the end, its ordered covering scan wins.
	dataHint := indexHint
	if len(driverLegs) >= 2 {
		dataHint = ""
	}

	dataSQL := fmt.Sprintf(
		"SELECT "+models.ImageRowColumns+`
		 FROM images i%s
		 WHERE %s
		 %s
		 LIMIT ? OFFSET ?`,
		dataHint, where, orderClause,
	)

	dataArgs := append(slices.Concat(args, orderArgs), limit, offset)
	rows, err := database.Read.Query(dataSQL, dataArgs...)
	if err != nil {
		return nil, fmt.Errorf("data query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var images []models.Image
	for rows.Next() {
		img, scanErr := models.ScanImageRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		images = append(images, img)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Page 1 fans synchronously: an async fan loses the race to the next
	// page-flip or prev/next. The fan drops the recent-id bound, which only
	// holds for this page, and a short fan corrects the fast counter's total.
	if q.CacheKey != "" && total > 0 && total <= adjacencyCacheMaxIDs {
		if len(images) == total {
			ids := make([]int64, len(images))
			for i, img := range images {
				ids[i] = img.ID
			}
			AdjacencyCacheSet(q.CacheKey, ids)
		} else if page == 1 && AdjacencyCacheTryAcquireFan(q.CacheKey) {
			defer AdjacencyCacheReleaseFan(q.CacheKey)
			fanWhere, fanArgs := where, args
			if idBounded {
				for i := range driverLegs {
					driverLegs[i].idBound = 0
				}
				fanWhere, fanArgs, _, _ = buildWhereDBDriverFull(q.Expr, database, driverLegs)
				fanWhere, fanArgs = applyAndDriver(fanWhere, fanArgs, driverLegs)
				fanWhere = andDefaultVisible(fanWhere, hasMissingFilter)
			}
			ctx, cancel := context.WithTimeout(context.Background(), fanBudget(time.Since(started)))
			ids := fetchSortedMatchIDs(ctx, database, indexHint, fanWhere, fanArgs, orderClause, orderArgs, total)
			cancel()
			if len(ids) > 0 {
				AdjacencyCacheSet(q.CacheKey, ids)
				if len(ids) < min(total, adjacencyCacheMaxIDs) {
					total = len(ids)
				}
			} else {
				AdjacencyCacheMarkFanOverBudget(q.CacheKey)
			}
		}
	}

	return &models.SearchResult{
		Page:    page,
		Limit:   limit,
		Total:   total,
		Results: images,
	}, nil
}

func executeFromCachedIDs(database *db.DB, ids []int64, page, limit int) (*models.SearchResult, error) {
	total := len(ids)
	offset := (page - 1) * limit
	if offset >= total {
		return &models.SearchResult{Page: page, Limit: limit, Total: total}, nil
	}
	end := min(offset+limit, total)
	pageIDs := ids[offset:end]

	placeholders, args := db.InPlaceholders(pageIDs)
	sql := fmt.Sprintf(
		"SELECT "+models.ImageRowColumns+" FROM images i WHERE i.id IN (%s)", placeholders,
	)
	rows, err := database.Read.Query(sql, args...)
	if err != nil {
		return nil, fmt.Errorf("cached id fetch: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byID := make(map[int64]models.Image, len(pageIDs))
	for rows.Next() {
		img, scanErr := models.ScanImageRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		byID[img.ID] = img
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]models.Image, 0, len(pageIDs))
	for _, id := range pageIDs {
		if img, ok := byID[id]; ok {
			out = append(out, img)
		}
	}
	return &models.SearchResult{
		Page:    page,
		Limit:   limit,
		Total:   total,
		Results: out,
	}, nil
}

// fetchSortedMatchIDs returns nil on any error, a cancelled ctx included:
// a truncated list would shrink the total and cut prev/next off mid-set.
func fetchSortedMatchIDs(ctx context.Context, database *db.DB, indexHint, where string, args []any, orderClause string, orderArgs []any, total int) []int64 {
	n := min(total, adjacencyCacheMaxIDs)
	sql := fmt.Sprintf(
		`SELECT i.id FROM images i%s WHERE %s %s LIMIT ?`,
		indexHint, where, orderClause,
	)
	qargs := append(slices.Concat(args, orderArgs), n)
	ids, err := db.QueryIDsContext(ctx, database.Read, sql, qargs...)
	if err != nil {
		return nil
	}
	return ids
}

// The random key has no index, so the cursor temp-sorts every match; an
// id bucket bounds that sort, at the cost of prev/next stopping at the
// bucket's edge.
const randomAdjacencyBucketSize = 2000

// Without a bucket the sort-key cursor can walk arbitrarily far between
// sparse matches. Wider than the random bucket because newest and
// filesize are the usual navigation sorts.
const andAdjacencyBucketSize = 10000

// orderCursor must match buildOrder's sort=order total order.
func orderCursor(series string, order sql.NullInt64, id int64, desc bool) (before, after, fwd, rev string, beforeArgs, afterArgs []any) {
	if desc {
		fwd = "ORDER BY i.series DESC, i.series_order IS NULL, i.series_order DESC, i.id DESC"
		rev = "ORDER BY i.series ASC, i.series_order IS NULL DESC, i.series_order ASC, i.id ASC"
		if order.Valid {
			before = "(i.series > ? OR (i.series = ? AND i.series_order IS NOT NULL AND (i.series_order, i.id) > (?, ?)))"
			after = "(i.series < ? OR (i.series = ? AND (i.series_order IS NULL OR (i.series_order, i.id) < (?, ?))))"
			beforeArgs = []any{series, series, order.Int64, id}
			afterArgs = []any{series, series, order.Int64, id}
		} else {
			before = "(i.series > ? OR (i.series = ? AND (i.series_order IS NOT NULL OR (i.series_order IS NULL AND i.id > ?))))"
			after = "(i.series < ? OR (i.series = ? AND i.series_order IS NULL AND i.id < ?))"
			beforeArgs = []any{series, series, id}
			afterArgs = []any{series, series, id}
		}
		return
	}
	fwd = "ORDER BY i.series ASC, i.series_order IS NULL, i.series_order ASC, i.id ASC"
	rev = "ORDER BY i.series DESC, i.series_order IS NULL DESC, i.series_order DESC, i.id DESC"
	if order.Valid {
		before = "(i.series < ? OR (i.series = ? AND i.series_order IS NOT NULL AND (i.series_order, i.id) < (?, ?)))"
		after = "(i.series > ? OR (i.series = ? AND (i.series_order IS NULL OR (i.series_order, i.id) > (?, ?))))"
		beforeArgs = []any{series, series, order.Int64, id}
		afterArgs = []any{series, series, order.Int64, id}
	} else {
		before = "(i.series < ? OR (i.series = ? AND (i.series_order IS NOT NULL OR (i.series_order IS NULL AND i.id < ?))))"
		after = "(i.series > ? OR (i.series = ? AND i.series_order IS NULL AND i.id > ?))"
		beforeArgs = []any{series, series, id}
		afterArgs = []any{series, series, id}
	}
	return
}

// The query must JOIN image_collections AS pc, and the order must match
// collectionOrderClause.
func collectionCursor(pos sql.NullInt64, id int64, desc bool) (before, after, fwd, rev string, beforeArgs, afterArgs []any) {
	if desc {
		fwd = "ORDER BY pc.position IS NULL, pc.position DESC, i.id DESC"
		rev = "ORDER BY pc.position IS NULL DESC, pc.position ASC, i.id ASC"
		if pos.Valid {
			before = "(pc.position IS NOT NULL AND (pc.position, i.id) > (?, ?))"
			after = "(pc.position IS NULL OR (pc.position, i.id) < (?, ?))"
		} else {
			before = "(pc.position IS NOT NULL OR (pc.position IS NULL AND i.id > ?))"
			after = "(pc.position IS NULL AND i.id < ?)"
		}
	} else {
		fwd = "ORDER BY pc.position IS NULL, pc.position ASC, i.id ASC"
		rev = "ORDER BY pc.position IS NULL DESC, pc.position DESC, i.id DESC"
		if pos.Valid {
			before = "(pc.position IS NOT NULL AND (pc.position, i.id) < (?, ?))"
			after = "(pc.position IS NULL OR (pc.position, i.id) > (?, ?))"
		} else {
			before = "(pc.position IS NOT NULL OR (pc.position IS NULL AND i.id < ?))"
			after = "(pc.position IS NULL AND i.id > ?)"
		}
	}
	if pos.Valid {
		beforeArgs = []any{pos.Int64, id}
		afterArgs = []any{pos.Int64, id}
	} else {
		beforeArgs = []any{id}
		afterArgs = []any{id}
	}
	return
}

type adjacencyPlan struct {
	where        string
	args         []any
	indexHint    string
	keyCol       string
	folderActive bool
	folderEq     string
	folderLo     string
	folderHi     string
	collOrder    bool
	prevCmp      string
	nextCmp      string
	prevSort     string
	nextSort     string
	prevArgs     []any
	nextArgs     []any
	ok           bool // false for an unseeded random sort
}

func buildAdjacencyPlan(ctx context.Context, database *db.DB, q Query, currentID int64, driverLegs []andDriverLeg, bucketed bool, bucketLo, bucketHi int64) (adjacencyPlan, error) {
	var p adjacencyPlan
	var ingestedAt string
	var fileSize int64
	var series string
	var seriesOrder sql.NullInt64
	if err := database.Read.QueryRowContext(ctx,
		`SELECT ingested_at, file_size, series, series_order FROM images WHERE id = ?`, currentID,
	).Scan(&ingestedAt, &fileSize, &series, &seriesOrder); err != nil {
		return p, err
	}

	where, args, hasMissingFilter, ceilingRewrote := buildWhereDBDriverFull(q.Expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)

	// The folder legs carry the folder predicate, so the per-image where
	// would repeat it.
	p.folderActive, p.folderEq, p.folderLo, p.folderHi = detectPureFolder(q.Expr)
	// sort=order has no single key column for the folder UNION-ALL legs.
	if q.Sort == "order" {
		p.folderActive = false
	}
	if p.folderActive {
		where = ""
		args = args[:0]
	}

	where = andDefaultVisible(where, hasMissingFilter)

	if bucketed {
		where = where + " AND i.id BETWEEN ? AND ?"
		args = append(args, bucketLo, bucketHi)
	}

	p.collOrder = q.Sort == "order" && q.OrderCollection != ""
	var pcPos sql.NullInt64
	if p.collOrder {
		// No membership reads as a NULL position, which sorts in the
		// trailing group.
		_ = database.Read.QueryRowContext(ctx,
			`SELECT position FROM image_collections WHERE image_id = ? AND name = ?`,
			currentID, q.OrderCollection).Scan(&pcPos)
	}

	if p.collOrder {
		before, after, fwd, rev, bArgs, aArgs := collectionCursor(pcPos, currentID, q.Order == "desc")
		p.prevCmp, p.prevSort, p.prevArgs = before, rev, bArgs
		p.nextCmp, p.nextSort, p.nextArgs = after, fwd, aArgs
	} else if q.Sort == "order" {
		before, after, fwd, rev, bArgs, aArgs := orderCursor(series, seriesOrder, currentID, q.Order == "desc")
		p.prevCmp, p.prevSort, p.prevArgs = before, rev, bArgs
		p.nextCmp, p.nextSort, p.nextArgs = after, fwd, aArgs
	} else {
		var keyVal any
		switch q.Sort {
		case "random":
			if q.RandomSeed == 0 {
				return p, nil
			}
			// %d emits only digits, so the seed interpolation is
			// injection-safe. db.RandomSortKey must hash as random_key()
			// does, or the cursor compares two key spaces.
			p.keyCol = fmt.Sprintf("random_key(i.id, %d)", q.RandomSeed)
			keyVal = int64(db.RandomSortKey(currentID, q.RandomSeed))
		case "filesize":
			p.keyCol = "i.file_size"
			keyVal = fileSize
		default: // "newest"
			p.keyCol = "i.ingested_at"
			keyVal = ingestedAt
		}

		// Row values, not the equivalent OR: only `(A, id) < (?, ?)`
		// seeks the (A, id) index.
		if q.Order == "asc" || q.Sort == "random" {
			p.prevCmp = fmt.Sprintf("(%s, i.id) < (?, ?)", p.keyCol)
			p.nextCmp = fmt.Sprintf("(%s, i.id) > (?, ?)", p.keyCol)
			p.prevSort = fmt.Sprintf("ORDER BY %s DESC, i.id DESC", p.keyCol)
			p.nextSort = fmt.Sprintf("ORDER BY %s ASC, i.id ASC", p.keyCol)
		} else {
			p.prevCmp = fmt.Sprintf("(%s, i.id) > (?, ?)", p.keyCol)
			p.nextCmp = fmt.Sprintf("(%s, i.id) < (?, ?)", p.keyCol)
			p.prevSort = fmt.Sprintf("ORDER BY %s ASC, i.id ASC", p.keyCol)
			p.nextSort = fmt.Sprintf("ORDER BY %s DESC, i.id DESC", p.keyCol)
		}
		p.prevArgs = []any{keyVal, currentID}
		p.nextArgs = []any{keyVal, currentID}
	}

	p.indexHint = sortIndexHint(q.Expr, q.Sort, hasMissingFilter, ceilingRewrote)
	p.where, p.args = where, args
	p.ok = true
	return p, nil
}

func ExecuteAdjacent(ctx context.Context, database *db.DB, q Query, currentID int64) (*int64, *int64, error) {
	if ids, ok := AdjacencyCacheGet(q.CacheKey); ok {
		prev, next := findInAdjacencyList(ids, currentID)
		return prev, next, nil
	}

	// No key column to seek on, so the neighbours come from the ranked list.
	if q.Sort == "similarity" {
		ids := similarityMatchIDs(ctx, database, q)
		if len(ids) == 0 {
			return nil, nil, nil
		}
		prev, next := findInAdjacencyList(ids, currentID)
		return prev, next, nil
	}

	// Decided first so the driver legs can be cut to the bucket: unbounded, an
	// INTERSECT of popular legs materialises far more rows than a bucket holds.
	// Small candidate sets skip it, as a bucket would rarely hold a second match.
	smallCandidate := false
	if total, ok := adjacencyTotalEstimate(database, q.Expr); ok && total < fastApproxThreshold {
		smallCandidate = true
	}

	bucketLo, bucketHi := int64(0), int64(0)
	bucketed := false
	switch {
	case smallCandidate:
	case q.Sort == "random" && containsTagPredicate(q.Expr):
		bucketLo = (currentID / randomAdjacencyBucketSize) * randomAdjacencyBucketSize
		bucketHi = bucketLo + randomAdjacencyBucketSize - 1
		bucketed = true
	case (q.Sort == "" || q.Sort == "newest" || q.Sort == "filesize") &&
		expensiveAdjacencyTags(q.Expr):
		bucketLo = (currentID / andAdjacencyBucketSize) * andAdjacencyBucketSize
		bucketHi = bucketLo + andAdjacencyBucketSize - 1
		bucketed = true
	}

	var driverLegs []andDriverLeg
	if !bucketed {
		driverLegs, _ = pickAndDriverTag(database, q.Expr, q.Sort == "random")
	} else {
		driverLegs, _ = pickAndDriverTag(database, q.Expr, true)
		for i := range driverLegs {
			driverLegs[i].idBound = bucketLo
			driverLegs[i].idBoundHi = bucketHi
		}
	}

	p, err := buildAdjacencyPlan(ctx, database, q, currentID, driverLegs, bucketed, bucketLo, bucketHi)
	if err != nil || !p.ok {
		return nil, nil, nil
	}

	lookup := func(cursorCmp, sort string, cursorArgs []any) *int64 {
		var sql string
		var qargs []any
		if p.collOrder {
			qargs = slices.Concat([]any{q.OrderCollection}, p.args, cursorArgs)
			sql = fmt.Sprintf(
				"SELECT i.id FROM images i JOIN image_collections pc ON pc.image_id = i.id AND pc.name = ? WHERE %s AND %s %s LIMIT 1",
				p.where, cursorCmp, sort)
		} else if p.folderActive {
			// Two seeks on idx_images_folder_nocase_visible; SQLite plans
			// the OR of the equality and the range as a full index scan
			// plus a temp sort.
			outer := strings.ReplaceAll(strings.ReplaceAll(sort, p.keyCol, "k"), "i.id", "id")
			legSQL := "SELECT i.id AS id, " + p.keyCol + " AS k FROM images i INDEXED BY idx_images_folder_nocase_visible WHERE %s AND " + p.where + " AND " + cursorCmp + " " + sort + " LIMIT 1"
			sql = "SELECT id FROM (SELECT * FROM (" + fmt.Sprintf(legSQL, "i.folder_path = ? COLLATE NOCASE") +
				") UNION ALL SELECT * FROM (" + fmt.Sprintf(legSQL, "i.folder_path >= ? COLLATE NOCASE AND i.folder_path < ? COLLATE NOCASE") +
				")) " + outer + " LIMIT 1"
			qargs = slices.Concat(
				[]any{p.folderEq}, p.args, cursorArgs,
				[]any{p.folderLo, p.folderHi}, p.args, cursorArgs,
			)
		} else {
			qargs = slices.Concat(p.args, cursorArgs)
			sql = fmt.Sprintf("SELECT i.id FROM images i%s WHERE %s AND %s %s LIMIT 1",
				p.indexHint, p.where, cursorCmp, sort)
		}
		var id int64
		if err := database.Read.QueryRow(sql, qargs...).Scan(&id); err != nil {
			return nil
		}
		return &id
	}
	return lookup(p.prevCmp, p.prevSort, p.prevArgs), lookup(p.nextCmp, p.nextSort, p.nextArgs), nil
}

// RankInQuery is 0-based and -1 when it has no useful answer. A sparse
// predicate can walk far before reaching the rank cap, so callers pass a
// deadline.
func RankInQuery(ctx context.Context, database *db.DB, q Query, currentID int64) (int, error) {
	// The cursor COUNT has no score column to compare against.
	if q.Sort == "similarity" {
		for i, id := range similarityMatchIDs(ctx, database, q) {
			if id == currentID {
				return i, nil
			}
		}
		return -1, nil
	}

	// A lone leaf is materialised only while that is cheaper than the
	// cursor's walk to currentID; random sort, with no index to walk,
	// takes it regardless.
	allowSingle := q.Sort == "random"
	if total, ok := fastTagTotal(database, q.Expr); !ok || total <= fastApproxThreshold {
		allowSingle = true
	}
	driverLegs, _ := pickAndDriverTag(database, q.Expr, allowSingle)

	// No bucket: the COUNT must see every row before currentID.
	p, err := buildAdjacencyPlan(ctx, database, q, currentID, driverLegs, false, 0, 0)
	if err != nil {
		return -1, err
	}
	if !p.ok {
		return -1, nil
	}

	var rows string
	var qargs []any
	if p.collOrder {
		rows = fmt.Sprintf(
			"SELECT 1 FROM images i JOIN image_collections pc ON pc.image_id = i.id AND pc.name = ? WHERE %s AND %s",
			p.where, p.prevCmp)
		qargs = slices.Concat([]any{q.OrderCollection}, p.args, p.prevArgs)
	} else if p.folderActive {
		legSQL := "SELECT 1 FROM images i INDEXED BY idx_images_folder_nocase_visible WHERE %s AND " + p.where + " AND " + p.prevCmp
		rows = fmt.Sprintf(legSQL, "i.folder_path = ? COLLATE NOCASE") +
			" UNION ALL " +
			fmt.Sprintf(legSQL, "i.folder_path >= ? COLLATE NOCASE AND i.folder_path < ? COLLATE NOCASE")
		qargs = slices.Concat(
			[]any{p.folderEq}, p.args, p.prevArgs,
			[]any{p.folderLo, p.folderHi}, p.args, p.prevArgs,
		)
	} else {
		rows = fmt.Sprintf(
			"SELECT 1 FROM images i%s WHERE %s AND %s",
			p.indexHint, p.where, p.prevCmp,
		)
		qargs = slices.Concat(p.args, p.prevArgs)
	}

	var rank int
	if err := database.Read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM ("+rows+" LIMIT ?)",
		append(qargs, rankInQueryMaxRank+1)...,
	).Scan(&rank); err != nil {
		return -1, err
	}
	if rank > rankInQueryMaxRank {
		return -1, nil
	}
	return rank, nil
}

type DeleteTarget struct {
	ID            int64
	CanonicalPath string
	FolderPath    string
	IsMissing     bool
}

func viewOrder(database *db.DB, expr Expr, sort, order string, seed int64, collection string) (string, []any, tags.OverlapSeed, bool) {
	switch {
	case sort == "order" && collection != "":
		clause, args := collectionOrderClause(collection, order)
		return clause, args, tags.OverlapSeed{}, false
	case sort == "similarity":
		if rankSeed, ok := similarityRankSeed(database, expr); ok {
			clause, args := similarityOrderClause(rankSeed, order)
			return clause, args, rankSeed, true
		}
	}
	return buildOrder(sort, order, seed), nil, tags.OverlapSeed{}, false
}

// Scope names images by query. Expr must already carry the viewer's ceiling,
// or the scope takes in rows the operator cannot see; delete-all walks it, so
// a term the executor honours and the scope ignores would delete everything.
type Scope struct {
	Expr Expr
	// Only for jobs that depend on position: a sort without a covering
	// index temp-sorts the whole set.
	ViewOrder       bool
	Sort            string
	Order           string
	RandomSeed      int64
	OrderCollection string
}

func (sc Scope) Stream(database *db.DB, visit func(DeleteTarget) error) error {
	order, orderArgs := "ORDER BY i.id", []any(nil)
	if sc.ViewOrder {
		order, orderArgs, _, _ = viewOrder(database, sc.Expr, sc.Sort, sc.Order, sc.RandomSeed, sc.OrderCollection)
	}
	return streamScope(database, sc.Expr, order, orderArgs, visit)
}

func (sc Scope) IDs(database *db.DB) ([]int64, error) {
	var ids []int64
	return ids, sc.Stream(database, func(t DeleteTarget) error {
		ids = append(ids, t.ID)
		return nil
	})
}

func streamScope(database *db.DB, expr Expr, orderBy string, orderArgs []any, visit func(DeleteTarget) error) error {
	driverLegs, _ := pickAndDriverTag(database, expr, false)
	where, args, hasMissingFilter, _ := buildWhereDBDriverFull(expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)
	where = andDefaultVisible(where, hasMissingFilter)

	rows, err := database.Read.Query(
		"SELECT i.id, i.canonical_path, i.folder_path, i.is_missing FROM images i WHERE "+where+" "+orderBy,
		append(args, orderArgs...)...,
	)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var t DeleteTarget
		var isMissing int
		if err := rows.Scan(&t.ID, &t.CanonicalPath, &t.FolderPath, &isMissing); err != nil {
			return err
		}
		t.IsMissing = isMissing == 1
		if err := visit(t); err != nil {
			return err
		}
	}
	return rows.Err()
}

const sidebarMaxPerCategory = 25

// SidebarTagsWithGlobalCount ranks by count on the page but reports the
// library-wide usage_count.
func SidebarTagsWithGlobalCount(database *db.DB, imageIDs []int64) ([]models.Tag, error) {
	if len(imageIDs) == 0 {
		return nil, nil
	}

	placeholders, args := db.InPlaceholders(imageIDs)

	return db.QueryAll(database.Read, tags.ScanTag,
		fmt.Sprintf(
			`WITH tag_counts AS (
			     SELECT t.id AS tag_id, t.name AS tag_name, tc.name AS cat_name,
			            tc.color AS cat_color, t.usage_count,
			            COUNT(DISTINCT it.image_id) AS page_count
			     FROM image_tags it INDEXED BY idx_image_tags_image
			     JOIN tags t ON t.id = it.tag_id
			     JOIN tag_categories tc ON tc.id = t.category_id
			     WHERE it.image_id IN (%s) AND t.is_alias = 0
			     GROUP BY t.id
			 )
			 SELECT tag_id, tag_name, cat_name, cat_color, usage_count
			 FROM (
			     SELECT tag_id, tag_name, cat_name, cat_color, usage_count, page_count,
			            ROW_NUMBER() OVER (PARTITION BY cat_name
			                               ORDER BY page_count DESC, tag_name ASC) AS rn
			     FROM tag_counts
			 )
			 WHERE rn <= ?
			 ORDER BY page_count DESC, tag_name ASC`,
			placeholders,
		),
		append(args, sidebarMaxPerCategory)...)
}

// The 10 suggestions shown, with headroom for the de-dup between the
// prefix and substring passes.
const suggestCandidateCap = 25

// Past this many context images the combination counts become lower bounds.
const suggestContextCap = 1000

// SuggestTagsWithFilter's UsageCount counts the images matching both expr
// and the tag.
func SuggestTagsWithFilter(database *db.DB, expr Expr, prefix, categoryName string, limit int) ([]models.Tag, error) {
	prefix = tags.NormalizeTagName(prefix)
	// With no context the combination count is the global usage count.
	if expr == nil {
		return tags.SuggestUsageRanked(database, prefix, categoryName, true, limit)
	}

	where, args, hasMissingFilter := buildWhereDB(expr, database)
	where = andDefaultVisible(where, hasMissingFilter)

	prefixPat := db.EscapeLike(prefix) + "%"
	substrPat := "%" + db.EscapeLike(prefix) + "%"

	// ctx is materialised once and each candidate probes image_tags
	// against it. (image_id, tag_id) is image_tags' key, so a plain COUNT
	// per tag counts distinct images.
	baseSQL := `WITH ctx AS (
	                SELECT i.id AS image_id FROM images i WHERE %s LIMIT ?
	            ),
	            cand AS (
	                SELECT id, category_id, usage_count
	                FROM tags
	                WHERE is_alias = 0
	                  AND name LIKE ? ESCAPE '\'
	                  %s
	                ORDER BY usage_count DESC
	                LIMIT ?
	            )
	            SELECT c.id, t.name, tc.name, tc.color, COUNT(it.image_id) AS combo
	            FROM cand c
	            JOIN tags t ON t.id = c.id
	            JOIN tag_categories tc ON tc.id = c.category_id
	            JOIN image_tags it ON it.tag_id = c.id
	                              AND it.image_id IN (SELECT image_id FROM ctx)
	            GROUP BY c.id
	            HAVING combo > 0
	            ORDER BY combo DESC, c.usage_count DESC
	            LIMIT ?`

	catClause := ""
	catArgs := []any{}
	if categoryName != "" {
		catClause = "AND category_id = (SELECT id FROM tag_categories WHERE name = ?)"
		catArgs = []any{categoryName}
	}

	run := func(pat string, prior []models.Tag, remaining int, nameNotLike string) ([]models.Tag, error) {
		extra := catClause
		qargs := make([]any, 0, 5+len(args)+len(catArgs))
		qargs = append(qargs, args...)
		qargs = append(qargs, suggestContextCap)
		qargs = append(qargs, pat)
		qargs = append(qargs, catArgs...)
		if nameNotLike != "" {
			extra = extra + ` AND name NOT LIKE ? ESCAPE '\'`
			qargs = append(qargs, nameNotLike)
		}
		qargs = append(qargs, suggestCandidateCap)
		qargs = append(qargs, remaining)
		rows, err := database.Read.Query(fmt.Sprintf(baseSQL, where, extra), qargs...)
		if err != nil {
			return prior, err
		}
		defer func() { _ = rows.Close() }()
		seen := map[int64]bool{}
		for _, t := range prior {
			seen[t.ID] = true
		}
		for rows.Next() {
			var t models.Tag
			var combo int
			if err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &combo); err != nil {
				return prior, err
			}
			if seen[t.ID] {
				continue
			}
			t.UsageCount = combo
			prior = append(prior, t)
			seen[t.ID] = true
		}
		return prior, rows.Err()
	}

	out, err := run(prefixPat, nil, limit, "")
	if err != nil {
		return nil, err
	}
	if len(out) < limit {
		out, err = run(substrPat, out, limit-len(out), prefixPat)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func sqlDir(order, def string) string {
	switch order {
	case "asc":
		return "ASC"
	case "desc":
		return "DESC"
	}
	return def
}

// Read from the join table: an image in several collections sorts by the
// pinned one's position, not its home order.
func collectionOrderClause(name, order string) (string, []any) {
	dir := sqlDir(order, "ASC")
	sub := "(SELECT position FROM image_collections WHERE image_id = i.id AND name = ?)"
	clause := "ORDER BY " + sub + " IS NULL, " + sub + " " + dir + ", i.id " + dir
	return clause, []any{name, name}
}

// PinnedCollectionName is "" unless exactly one collection: value is
// ANDed at the top level; collection:any names no collection.
func PinnedCollectionName(expr Expr) string {
	seen := map[string]struct{}{}
	last := ""
	var walk func(Expr)
	walk = func(e Expr) {
		switch v := e.(type) {
		case AndExpr:
			walk(v.Left)
			walk(v.Right)
		case FilterExpr:
			if v.Key == "collection" && v.Val != "" && !strings.EqualFold(v.Val, "any") {
				seen[strings.ToLower(v.Val)] = struct{}{}
				last = v.Val
			}
		}
	}
	walk(expr)
	if len(seen) == 1 {
		return last
	}
	return ""
}

// Collection order reads forwards, 1..N; every other sort leads with the
// newest or largest.
func DefaultOrder(sort string) string {
	if sort == "order" {
		return "asc"
	}
	return "desc"
}

func buildOrder(sort, order string, randomSeed int64) string {
	switch sort {
	case "filesize":
		dir := sqlDir(order, "DESC")
		return "ORDER BY i.file_size " + dir + ", i.id " + dir
	case "order":
		// NULL positions sort last in both directions; id makes the order
		// total.
		dir := "ASC"
		if order == "desc" {
			dir = "DESC"
		}
		return "ORDER BY i.series " + dir + ", i.series_order IS NULL, i.series_order " + dir + ", i.id " + dir
	case "random":
		if randomSeed != 0 {
			// %d emits only digits, so the seed interpolation is
			// injection-safe.
			return fmt.Sprintf("ORDER BY random_key(i.id, %d), i.id", randomSeed)
		}
		return "ORDER BY RANDOM(), i.id"
	default: // "newest"
		dir := sqlDir(order, "DESC")
		return "ORDER BY i.ingested_at " + dir + ", i.id " + dir
	}
}
