package search

import (
	"database/sql"
	"strings"

	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/tags"
)

// fastTagTotal answers exactly or with an upper bound, never under: an
// over-count only leaves empty trailing pages.
func fastTagTotal(database *db.DB, expr Expr) (int, bool) {
	if n, ok := fastCountCeiling(database, expr); ok {
		return n, true
	}
	switch e := expr.(type) {
	case TagExpr:
		return fastCountTag(database, e)
	case NotExpr:
		return fastCountNot(database, e)
	case AndExpr:
		return fastCountAnd(database, e)
	case OrExpr:
		return fastCountOr(database, e)
	case FilterExpr:
		return fastCountFilter(database, e)
	}
	return 0, false
}

// Unlike fastTagTotal, AND and OR are not gated here: the bucket decision
// needs exactly the small intersections that gate hides.
func adjacencyTotalEstimate(database *db.DB, expr Expr) (int, bool) {
	switch e := expr.(type) {
	case AndExpr:
		return fastCountBinary(database, e.Left, e.Right, adjacencyTotalEstimate, minInt)
	case OrExpr:
		sum, ok := fastCountBinary(database, e.Left, e.Right, adjacencyTotalEstimate, addInts)
		if !ok {
			return 0, false
		}
		return capToVisible(database, sum)
	}
	return fastTagTotal(database, expr)
}

// Counts rating_rank, exact for an image with several rating tags, which
// subtracting usage_counts would take out once per level. A user search
// ANDed on gets the exact COUNT: a bound would advertise phantom pages.
func fastCountCeiling(database *db.DB, expr Expr) (int, bool) {
	user, rank, ok := extractCeilingShape(expr)
	if !ok || user != nil {
		return 0, false
	}
	// Pinned: rating_rank has five values, which the sampled sqlite_stat1
	// underestimates, and unpinned the planner counts through
	// idx_images_missing, reading every visible row.
	var total int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images INDEXED BY idx_images_rating_rank_visible
		 WHERE is_missing = 0 AND rating_rank <= ?`, rank,
	).Scan(&total); err != nil {
		return 0, false
	}
	return total, true
}

// Only negations excluding every level above some rank equal rating_rank
// <= rank; any other rating negation stays in user with its own meaning.
func extractCeilingShape(expr Expr) (Expr, int, bool) {
	if expr == nil {
		return nil, 0, false
	}
	excluded := map[string]bool{}
	WalkAndedLeaves(expr, func(leaf Expr) {
		if level, ok := ratingNegation(leaf); ok {
			excluded[level] = true
		}
	})
	top := len(tags.RatingLevels) - 1
	rank := top
	for rank >= 0 && excluded[tags.RatingLevels[rank]] {
		rank--
	}
	if rank == top {
		return nil, 0, false
	}
	return peelCeilingChain(expr, rank), rank, true
}

func ratingNegation(expr Expr) (string, bool) {
	if n, ok := expr.(NotExpr); ok {
		if f, ok := n.Expr.(FilterExpr); ok && f.Key == "rating" {
			return strings.ToLower(f.Val), true
		}
	}
	return "", false
}

func peelCeilingChain(expr Expr, rank int) Expr {
	switch v := expr.(type) {
	case NotExpr:
		if level, ok := ratingNegation(v); ok && tags.RatingRank(level) > rank {
			return nil
		}
		return expr
	case AndExpr:
		left := peelCeilingChain(v.Left, rank)
		right := peelCeilingChain(v.Right, rank)
		switch {
		case left == nil && right == nil:
			return nil
		case left == nil:
			return right
		case right == nil:
			return left
		default:
			return AndExpr{Left: left, Right: right}
		}
	}
	return expr
}

// Under this many rows the exact COUNT is fast enough, so the bounded
// shortcuts stand down.
const fastApproxThreshold = 50000

// Five default pages. A rank costs its depth, prev/next strays a page or
// two from where the operator arrived, and deeper than that the URL's
// page is already right.
const rankInQueryMaxRank = 200

func fastCountTag(database *db.DB, t TagExpr) (int, bool) {
	if t.Tag == "" {
		return 0, false
	}
	pred, arg, ok := tagNamePredicate("name", t.Wildcard, t.Tag)
	if !ok {
		return 0, false
	}
	canonIDs, err := db.QueryIDs(database.Read,
		`SELECT DISTINCT COALESCE(canonical_tag_id, id) FROM tags WHERE `+pred,
		arg,
	)
	if err != nil {
		return 0, false
	}
	if len(canonIDs) == 0 {
		return 0, true
	}
	if len(canonIDs) == 1 {
		var n int
		if err := database.Read.QueryRow(
			`SELECT usage_count FROM tags WHERE id = ?`, canonIDs[0],
		).Scan(&n); err != nil {
			return 0, false
		}
		return n, true
	}
	// Same name in two categories: a sum over-counts, and an exact name
	// wants an exact count.
	if t.Wildcard == "" {
		return 0, false
	}
	var n int
	if err := database.Read.QueryRow(
		`SELECT COALESCE(SUM(usage_count), 0) FROM tags WHERE id IN (` + inlineIDs(canonIDs) + `)`,
	).Scan(&n); err != nil {
		return 0, false
	}
	if n < fastApproxThreshold {
		return 0, false
	}
	// Summed per tag, a broad pattern can total several times the library.
	return capToVisible(database, n)
}

// Exact counts only: visible minus an upper bound would under-count and
// hide real pages.
func fastCountNot(database *db.DB, e NotExpr) (int, bool) {
	inner, ok := e.Expr.(TagExpr)
	if !ok || inner.Wildcard != "" || inner.Tag == "" {
		return 0, false
	}
	used, ok := fastCountTag(database, inner)
	if !ok {
		return 0, false
	}
	visible, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	return max(visible-used, 0), true
}

func fastCountBinary(database *db.DB, l, r Expr, recurse func(*db.DB, Expr) (int, bool), combine func(int, int) int) (int, bool) {
	lv, ok := recurse(database, l)
	if !ok {
		return 0, false
	}
	rv, ok := recurse(database, r)
	if !ok {
		return 0, false
	}
	return combine(lv, rv), true
}

func addInts(a, b int) int { return a + b }

func minInt(a, b int) int { return min(a, b) }

func capToVisible(database *db.DB, sum int) (int, bool) {
	v, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	return min(sum, v), true
}

func fastCountAnd(database *db.DB, e AndExpr) (int, bool) {
	minN, ok := fastCountBinary(database, e.Left, e.Right, fastTagTotal, minInt)
	if !ok || minN < fastApproxThreshold {
		return 0, false
	}
	return minN, true
}

func fastCountOr(database *db.DB, e OrExpr) (int, bool) {
	sum, ok := fastCountBinary(database, e.Left, e.Right, fastTagTotal, addInts)
	if !ok || sum < fastApproxThreshold {
		return 0, false
	}
	return capToVisible(database, sum)
}

var fastCountFilters = map[string]func(*db.DB, FilterExpr) (int, bool){
	"cat":        fastCountCat,
	"generated":  fastCountGenerated,
	"rating":     fastCountRating,
	"tagged":     fastCountTagged,
	"autotagged": fastCountTagged,
	"inbox":      fastCountInbox,
	"ai":         fastCountAI,
	"folder":     fastCountFolder,
	"lookup":     fastCountLookup,
}

// lookup:never is visible minus looked-up: its NOT EXISTS walks the whole
// library on a gallery that never ran a lookup. The other values' EXISTS
// is already selective.
func fastCountLookup(database *db.DB, e FilterExpr) (int, bool) {
	if strings.ToLower(e.Val) != "never" {
		return 0, false
	}
	visible, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	var recorded int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM (SELECT DISTINCT image_id FROM image_lookups) l
		 JOIN images i ON i.id = l.image_id AND i.is_missing = 0`,
	).Scan(&recorded); err != nil {
		return 0, false
	}
	return max(visible-recorded, 0), true
}

func fastCountCat(database *db.DB, e FilterExpr) (int, bool) {
	var n int
	if err := database.Read.QueryRow(
		`SELECT COALESCE(SUM(usage_count), 0) FROM tags
		 WHERE is_alias = 0
		   AND category_id = (SELECT id FROM tag_categories WHERE name = ?)`,
		e.Val,
	).Scan(&n); err != nil {
		return 0, false
	}
	if n < fastApproxThreshold {
		return 0, false
	}
	return n, true
}

func fastCountFilter(database *db.DB, e FilterExpr) (int, bool) {
	if e.Val == "" {
		return 0, false
	}
	if fn, ok := fastCountFilters[e.Key]; ok {
		return fn(database, e)
	}
	if searchkw.IsKeyword(e.Key) {
		return 0, false
	}
	catID, ok, err := tags.CategoryIDByName(database, e.Key)
	if !ok || err != nil {
		return 0, false
	}
	var n int
	err = database.Read.QueryRow(
		`SELECT canon.usage_count FROM tags t
		 JOIN tags canon ON canon.id = COALESCE(t.canonical_tag_id, t.id)
		 WHERE t.name = ? AND t.category_id = ?
		 LIMIT 1`,
		tags.NormalizeTagName(e.Val), catID,
	).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, true
	}
	if err != nil {
		return 0, false
	}
	return n, true
}

// usage_count bounds rating:X from above, exactly for the top level; an
// image carrying two ratings only over-counts.
func fastCountRating(database *db.DB, e FilterExpr) (int, bool) {
	level := strings.ToLower(e.Val)
	rank := tags.RatingRank(level)
	if rank < 0 {
		return 0, true
	}
	var n int
	err := database.Read.QueryRow(
		`SELECT t.usage_count FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.is_alias = 0 AND t.name = ?`,
		level,
	).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, true
	}
	if err != nil {
		return 0, false
	}
	// Zero and the top level are exact; other levels are bounds and wait
	// for the threshold.
	if n == 0 || rank == len(tags.RatingLevels)-1 {
		return n, true
	}
	if n < fastApproxThreshold {
		return 0, false
	}
	return n, true
}

// '0' sorts right after '/', so [X/, X0) holds exactly the paths under X.
func fastCountFolder(database *db.DB, e FilterExpr) (int, bool) {
	rangeLo := e.Val + "/"
	rangeHi := e.Val + "0"
	var n int
	if err := database.Read.QueryRow(
		`SELECT (
		     (SELECT COUNT(*) FROM images
		        WHERE folder_path = ? COLLATE NOCASE AND is_missing = 0)
		   + (SELECT COUNT(*) FROM images INDEXED BY idx_images_folder_nocase_visible
		        WHERE folder_path >= ? COLLATE NOCASE
		          AND folder_path < ? COLLATE NOCASE
		          AND is_missing = 0)
		 )`,
		e.Val, rangeLo, rangeHi,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// The LIKE-OR matches no index, so the few distinct source_type values
// are matched in Go and the count takes an IN over them.
func fastCountAI(database *db.DB, e FilterExpr) (int, bool) {
	val := strings.ToLower(e.Val)
	if val == "sd" {
		val = "a1111"
	}
	if val == "any" || val == "none" {
		return 0, false
	}
	if !strings.Contains(val, ",") {
		return 0, false
	}
	present, err := db.QueryStrings(database.Read, `SELECT DISTINCT source_type FROM images`)
	if err != nil {
		return 0, false
	}

	// Must accept exactly what buildAIFilter's four LIKEs do.
	prefix := val + ","
	suffix := "," + val
	middle := "," + val + ","
	var matching []string
	for _, s := range present {
		if s == val ||
			strings.HasPrefix(s, prefix) ||
			strings.HasSuffix(s, suffix) ||
			strings.Contains(s, middle) {
			matching = append(matching, s)
		}
	}
	if len(matching) == 0 {
		return 0, true
	}
	placeholders, args := db.InPlaceholders(matching)
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images WHERE source_type IN (`+placeholders+`) AND is_missing = 0`,
		args...,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

func parseBoolVal(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "y", "on", "1":
		return true, true
	case "false", "no", "n", "off", "0":
		return false, true
	}
	return false, false
}

// Visible minus the cached untagged count; the NOT EXISTS behind it is
// too slow to run per search.
func fastCountTagged(database *db.DB, e FilterExpr) (int, bool) {
	if e.Val != "" {
		val, ok := parseBoolVal(e.Val)
		if !ok || !val {
			return 0, false
		}
	}
	visible, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	var untagged int
	if e.Key == "autotagged" {
		untagged, ok = counts.AutoUntaggedVisibleCount(database)
	} else {
		untagged, ok = counts.UntaggedVisibleCount(database)
	}
	if !ok {
		return 0, false
	}
	return max(visible-untagged, 0), true
}

// Ungated: idx_images_inbox_visible is exact at any size.
func fastCountInbox(database *db.DB, e FilterExpr) (int, bool) {
	val, ok := parseBoolVal(e.Val)
	if !ok {
		// The filter emits 1=0 for a non-boolean, so zero is exact.
		return 0, true
	}
	target := 0
	if val {
		target = 1
	}
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images WHERE is_missing = 0 AND is_inbox = ?`, target,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// Seeks the genhash indexes instead of an EXISTS per visible image.
// UNION, not UNION ALL: an image can carry both metadata kinds.
func fastCountGenerated(database *db.DB, e FilterExpr) (int, bool) {
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM (
		     SELECT sm.image_id FROM sd_metadata sm
		       JOIN images i ON i.id = sm.image_id
		       WHERE sm.generation_hash = ? AND i.is_missing = 0
		     UNION
		     SELECT cm.image_id FROM comfyui_metadata cm
		       JOIN images i ON i.id = cm.image_id
		       WHERE cm.generation_hash = ? AND i.is_missing = 0
		 )`,
		e.Val, e.Val,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

func fastVisibleCount(database *db.DB) (int, bool) { return counts.VisibleCount(database) }
