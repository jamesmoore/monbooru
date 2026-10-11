package library

import (
	"sync"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
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

func (c *Ceiling) Apply(userExpr search.Expr) search.Expr {
	return search.ApplyCeiling(userExpr, c.Level())
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

func (c *Ceiling) RankFilter() gallery.RankCeiling {
	rank, active := c.RankCeiling()
	return gallery.RankCeiling{Rank: rank, Active: active}
}

// TaintedAmong reads, in one query, the over-ceiling ids among the
// endpoints of table's colA and colB. A read error keeps every row visible
// rather than failing the page.
func (c *Ceiling) TaintedAmong(table, colA, colB string) map[int64]bool {
	rank, active := c.RankCeiling()
	if !active || c.cx == nil || c.cx.DB == nil {
		return nil
	}
	tainted := map[int64]bool{}
	err := db.QueryIDsFunc(c.cx.DB.Read, func(id int64) bool {
		tainted[id] = true
		return true
	}, `SELECT id FROM images WHERE rating_rank > ? AND id IN (
		SELECT `+colA+` FROM `+table+` UNION SELECT `+colB+` FROM `+table+`)`, rank)
	if err != nil {
		logx.Debugf("ceiling tainted read: %v", err)
		return nil
	}
	return tainted
}

// g may be nil: Apply still works, ExcludedTagIDs and TaintedAmong
// return nil.
func NewCeiling(level string, g *Gallery) *Ceiling {
	return &Ceiling{level: level, cx: g}
}
