// Package counts caches whole-library tallies per gallery.
package counts

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

type cache struct {
	db                  *db.DB
	untaggedVisible     atomic.Pointer[int]
	autoUntaggedVisible atomic.Pointer[int]
	// Cheap, but the tag-pairs pass divides by it once per image.
	visibleCount     atomic.Pointer[int]
	countedTags      atomic.Pointer[CountedTags]
	inboxCount       atomic.Pointer[int]
	tagCount         atomic.Pointer[int]
	collectionsCount atomic.Pointer[int]
	phashMissing     atomic.Pointer[int]
}

var (
	mu     sync.Mutex
	caches = map[*db.DB]*cache{}
)

func forDB(database *db.DB) *cache {
	mu.Lock()
	defer mu.Unlock()
	c, ok := caches[database]
	if !ok {
		c = &cache{db: database}
		caches[database] = c
	}
	return c
}

func Release(database *db.DB) {
	mu.Lock()
	delete(caches, database)
	mu.Unlock()
}

func (c *cache) cachedCount(slot *atomic.Pointer[int], sql string) (int, bool) {
	if p := slot.Load(); p != nil {
		return *p, true
	}
	var n int
	if err := c.db.Read.QueryRow(sql).Scan(&n); err != nil {
		return 0, false
	}
	slot.Store(&n)
	return n, true
}

// UntaggedVisibleCount must leave out the derived meta rows exactly as
// the tagged: filter does, or the header count disagrees with the grid.
func UntaggedVisibleCount(database *db.DB) (int, bool) {
	c := forDB(database)
	return c.cachedCount(&c.untaggedVisible,
		`SELECT COUNT(*) FROM images i
		 WHERE is_missing = 0
		   AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id
		                    AND it.tagger_name IS NOT '`+models.TagSourceMonbooru+`')`)
}

func VisibleCount(database *db.DB) (int, bool) {
	c := forDB(database)
	return c.cachedCount(&c.visibleCount, `SELECT COUNT(*) FROM images WHERE is_missing = 0`)
}

func AutoUntaggedVisibleCount(database *db.DB) (int, bool) {
	c := forDB(database)
	return c.cachedCount(&c.autoUntaggedVisible,
		`SELECT COUNT(*) FROM images i
		 WHERE is_missing = 0
		   AND NOT EXISTS (
		         SELECT 1 FROM image_tags it
		         WHERE it.image_id = i.id AND it.is_auto = 1
		       )`)
}

func InboxCount(database *db.DB) (int, bool) {
	c := forDB(database)
	return c.cachedCount(&c.inboxCount, `SELECT COUNT(*) FROM images WHERE is_missing = 0 AND is_inbox = 1`)
}

func TagCount(database *db.DB) (int, bool) {
	c := forDB(database)
	return c.cachedCount(&c.tagCount, `SELECT COUNT(*) FROM tags WHERE is_alias = 0`)
}

func CollectionsCount(database *db.DB) (int, bool) {
	c := forDB(database)
	return c.cachedCount(&c.collectionsCount, `SELECT COUNT(*) FROM collection_counts WHERE visible_count > 0`)
}

func PhashMissing(database *db.DB) (int, bool) {
	c := forDB(database)
	return c.cachedCount(&c.phashMissing, `SELECT COUNT(*) FROM images WHERE phash IS NULL AND is_missing = 0`)
}

// InvalidatePhashMissing is for phash writes that leave image membership
// alone; anything else calls Invalidate.
func InvalidatePhashMissing(database *db.DB) {
	forDB(database).phashMissing.Store(nil)
}

// CountedTags keeps parallel id-sorted slices rather than a map: a million
// images cost 12 MB here against about four times that in map buckets.
type CountedTags struct {
	maxUsage int64
	ids      []int64
	totals   []int32
	used     atomic.Int64
}

func (c *CountedTags) Total(id int64) int32 {
	if i, ok := slices.BinarySearch(c.ids, id); ok {
		return c.totals[i]
	}
	return 0
}

func CountedTagTotals(ctx context.Context, database *db.DB, maxUsage int64) (*CountedTags, error) {
	c := forDB(database)
	if t := c.countedTags.Load(); t != nil && t.maxUsage == maxUsage {
		t.used.Store(time.Now().UnixNano())
		return t, nil
	}
	rows, err := database.Read.QueryContext(ctx,
		`SELECT it.image_id, count(*)
		   FROM image_tags it
		   JOIN tags t ON t.id = it.tag_id
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE tc.name != 'meta' AND t.usage_count <= ?
		  GROUP BY it.image_id`, maxUsage)
	if err != nil {
		return nil, fmt.Errorf("counted tag totals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	totals := &CountedTags{maxUsage: maxUsage}
	for rows.Next() {
		var id int64
		var total int32
		if err := rows.Scan(&id, &total); err != nil {
			return nil, err
		}
		totals.ids = append(totals.ids, id)
		totals.totals = append(totals.totals, total)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	totals.used.Store(time.Now().UnixNano())
	c.countedTags.Store(totals)
	return totals, nil
}

func ReleaseIdleCountedTags(database *db.DB, after time.Duration) bool {
	c := forDB(database)
	t := c.countedTags.Load()
	if t == nil || time.Since(time.Unix(0, t.used.Load())) < after {
		return false
	}
	return c.countedTags.CompareAndSwap(t, nil)
}

// Invalidate must follow every write that changes image_tags membership.
func Invalidate(database *db.DB) {
	c := forDB(database)
	c.untaggedVisible.Store(nil)
	c.autoUntaggedVisible.Store(nil)
	c.visibleCount.Store(nil)
	c.countedTags.Store(nil)
	c.inboxCount.Store(nil)
	c.tagCount.Store(nil)
	c.collectionsCount.Store(nil)
	c.phashMissing.Store(nil)
}
