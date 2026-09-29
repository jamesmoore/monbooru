package library

import (
	"sync"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tags"
)

// Ceiling is shared by one request's goroutines; mu guards the lazy cache.
type Ceiling struct {
	level string
	cx    *Gallery

	mu             sync.Mutex
	excludedIDs    []int64
	excludedLoaded bool
}

// Level is the raw cookie value; "explicit" means no ceiling too, so gate
// on IsActive.
func (c *Ceiling) Level() string {
	if c == nil {
		return ""
	}
	return c.level
}

func (c *Ceiling) IsActive() bool {
	if c == nil {
		return false
	}
	return c.level != "" && c.level != "explicit"
}

// The executor turns this chain back into one rating_rank comparison, so it
// must keep excluding every level above the ceiling.
func (c *Ceiling) Apply(userExpr search.Expr) search.Expr {
	if c == nil || !c.IsActive() {
		return userExpr
	}
	rank := tags.RatingRank(c.level)
	if rank < 0 || rank >= len(tags.RatingLevels)-1 {
		return userExpr
	}
	var ce search.Expr
	for i := rank + 1; i < len(tags.RatingLevels); i++ {
		not := search.NotExpr{Expr: search.FilterExpr{Key: "rating", Val: tags.RatingLevels[i]}}
		if ce == nil {
			ce = not
		} else {
			ce = search.AndExpr{Left: ce, Right: not}
		}
	}
	if userExpr == nil {
		return ce
	}
	return search.AndExpr{Left: userExpr, Right: ce}
}

func (c *Ceiling) ExcludedTagIDs() []int64 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.excludedLoaded {
		return c.excludedIDs
	}
	if !c.IsActive() {
		return nil
	}
	c.excludedLoaded = true
	if c.cx == nil || c.cx.TagSvc == nil {
		return nil
	}
	c.excludedIDs = c.cx.TagSvc.RatingTagIDsAbove(c.level)
	return c.excludedIDs
}

func notExistsRatingPredicate(col, in string) string {
	return `NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = ` + col + ` AND it.tag_id IN (` + in + `))`
}

// Empty when inactive, so the caller can omit the WHERE and keep the
// covering scan.
func (c *Ceiling) WhereOne(col string) (string, []any) {
	ids := c.ExcludedTagIDs()
	if len(ids) == 0 {
		return "", nil
	}
	in, args := db.InPlaceholders(ids)
	return notExistsRatingPredicate(col, in), args
}

func (c *Ceiling) WhereTwo(leftCol, rightCol string) (string, []any) {
	ids := c.ExcludedTagIDs()
	if len(ids) == 0 {
		return "", nil
	}
	in, a := db.InPlaceholders(ids)
	args := append(append([]any{}, a...), a...)
	return notExistsRatingPredicate(leftCol, in) + " AND " + notExistsRatingPredicate(rightCol, in), args
}

func (c *Ceiling) WhereGroupClean(membersTable, groupCol string) (string, []any) {
	ids := c.ExcludedTagIDs()
	if len(ids) == 0 {
		return "", nil
	}
	in, args := db.InPlaceholders(ids)
	return `NOT EXISTS (
		SELECT 1 FROM ` + membersTable + ` mr
		JOIN image_tags it ON it.image_id = mr.image_id
		WHERE mr.group_id = ` + groupCol + ` AND it.tag_id IN (` + in + `)
	)`, args
}

// Rows pass with rank_col <= rank, so the unrated -1 passes every level.
func (c *Ceiling) RankCeiling() (int, bool) {
	if c == nil || !c.IsActive() {
		return 0, false
	}
	return tags.RatingRank(c.level), true
}

// Probing the few ids at hand beats preloading every over-ceiling image.
// A read error keeps the row visible rather than failing the page.
func (c *Ceiling) AnyTainted(ids []int64) bool {
	rank, active := c.RankCeiling()
	if !active || len(ids) == 0 || c.cx == nil || c.cx.DB == nil {
		return false
	}
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		in, args := db.InPlaceholders(ids[start:end])
		var tainted int
		err := c.cx.DB.Read.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM images WHERE id IN (`+in+`) AND rating_rank > ?)`,
			append(args, rank)...,
		).Scan(&tainted)
		if err != nil {
			logx.Debugf("ceiling tainted probe: %v", err)
			return false
		}
		if tainted != 0 {
			return true
		}
	}
	return false
}

// g may be nil: Apply still works, ExcludedTagIDs returns nil and
// AnyTainted false.
func NewCeiling(level string, g *Gallery) *Ceiling {
	return &Ceiling{level: level, cx: g}
}
