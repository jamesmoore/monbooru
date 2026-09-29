package search

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Worst-case cache memory: 4 entries x 1M ids x 8 bytes = 32 MB.
const (
	adjacencyCacheTTL        = 5 * time.Minute
	adjacencyCacheMaxEntries = 4
	adjacencyCacheMaxIDs     = 1000000
	// The fan has no LIMIT to stop at. It gets twice what the request has
	// spent, and at least the floor: a query slow once repays its fan, and
	// an overrun only leaves the entry unset.
	adjacencyFanBudget    = 750 * time.Millisecond
	adjacencyFanCostRatio = 2
)

func fanBudget(spent time.Duration) time.Duration {
	return max(adjacencyFanBudget, spent*adjacencyFanCostRatio)
}

type adjacencyCacheEntry struct {
	ids       []int64
	expiresAt time.Time
}

var (
	adjCacheMu      sync.Mutex
	adjCacheEntries = make(map[string]adjacencyCacheEntry)
	adjCacheOrder   []string

	// One fan per key at a time: concurrent misses would each run the
	// same full SELECT.
	fanInFlightMu sync.Mutex
	fanInFlight   = map[string]bool{}

	// Keys whose last fan came back empty, until they may try again: without
	// the hold-off such a query pays the whole budget on every page-1 hit.
	fanOverBudget = map[string]time.Time{}
)

// AdjacencyCacheTryAcquireFan's winner must call
// AdjacencyCacheReleaseFan, whatever the outcome.
func AdjacencyCacheTryAcquireFan(key string) bool {
	if key == "" {
		return false
	}
	fanInFlightMu.Lock()
	defer fanInFlightMu.Unlock()
	if fanInFlight[key] {
		return false
	}
	if retryAt, held := fanOverBudget[key]; held {
		if time.Now().Before(retryAt) {
			return false
		}
		delete(fanOverBudget, key)
	}
	fanInFlight[key] = true
	return true
}

func AdjacencyCacheMarkFanOverBudget(key string) {
	if key == "" {
		return
	}
	fanInFlightMu.Lock()
	fanOverBudget[key] = time.Now().Add(adjacencyCacheTTL)
	fanInFlightMu.Unlock()
}

func AdjacencyCacheReleaseFan(key string) {
	fanInFlightMu.Lock()
	delete(fanInFlight, key)
	fanInFlightMu.Unlock()
}

// AdjacencyCacheGet returns the cached array itself: callers must not
// modify it.
func AdjacencyCacheGet(key string) ([]int64, bool) {
	if key == "" {
		return nil, false
	}
	adjCacheMu.Lock()
	defer adjCacheMu.Unlock()
	entry, ok := adjCacheEntries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(adjCacheEntries, key)
		removeFromOrder(key)
		return nil, false
	}
	return entry.ids, true
}

// AdjacencyCacheSet takes a re-set key out of its old LRU slot, whose
// eviction would otherwise delete the fresh entry.
func AdjacencyCacheSet(key string, ids []int64) {
	if key == "" || len(ids) == 0 || len(ids) > adjacencyCacheMaxIDs {
		return
	}
	adjCacheMu.Lock()
	defer adjCacheMu.Unlock()
	if _, exists := adjCacheEntries[key]; exists {
		removeFromOrder(key)
	}
	adjCacheOrder = append(adjCacheOrder, key)
	snapshot := make([]int64, len(ids))
	copy(snapshot, ids)
	adjCacheEntries[key] = adjacencyCacheEntry{
		ids:       snapshot,
		expiresAt: time.Now().Add(adjacencyCacheTTL),
	}
	for len(adjCacheOrder) > adjacencyCacheMaxEntries {
		oldest := adjCacheOrder[0]
		adjCacheOrder = adjCacheOrder[1:]
		delete(adjCacheEntries, oldest)
	}
}

func AdjacencyCacheDropForGallery(gallery string) {
	if gallery == "" {
		return
	}
	prefix := gallery + "\x00"
	adjCacheDrop(func(k string, _ adjacencyCacheEntry) bool {
		return strings.HasPrefix(k, prefix)
	})
	dropFanHoldOffs(func(k string, _ time.Time) bool {
		return strings.HasPrefix(k, prefix)
	})
}

// AdjacencyCacheSweep exists because Get and Set evict only what they
// touch: an idle process would keep expired lists, and the hold-offs of
// keys nobody asks for again would pile up.
func AdjacencyCacheSweep() {
	now := time.Now()
	adjCacheDrop(func(_ string, entry adjacencyCacheEntry) bool {
		return now.After(entry.expiresAt)
	})
	dropFanHoldOffs(func(_ string, retryAt time.Time) bool {
		return now.After(retryAt)
	})
}

func dropFanHoldOffs(drop func(string, time.Time) bool) {
	fanInFlightMu.Lock()
	maps.DeleteFunc(fanOverBudget, drop)
	fanInFlightMu.Unlock()
}

func adjCacheDrop(drop func(string, adjacencyCacheEntry) bool) {
	adjCacheMu.Lock()
	defer adjCacheMu.Unlock()
	maps.DeleteFunc(adjCacheEntries, drop)
	adjCacheOrder = slices.DeleteFunc(adjCacheOrder, func(k string) bool {
		_, kept := adjCacheEntries[k]
		return !kept
	})
}

func removeFromOrder(key string) {
	if i := slices.Index(adjCacheOrder, key); i >= 0 {
		adjCacheOrder = slices.Delete(adjCacheOrder, i, i+1)
	}
}

// BuildAdjacencyCacheKey joins on NUL so no component can run into the
// next. The seed counts only under random sort, so a leftover seed param
// can't split a newest or filesize entry.
func BuildAdjacencyCacheKey(gallery, query, sort, order string, seed int64, ceiling string) string {
	seedStr := ""
	if sort == "random" && seed != 0 {
		seedStr = strconv.FormatInt(seed, 10)
	}
	return strings.Join([]string{gallery, query, sort, order, seedStr, ceiling}, "\x00")
}

func findInAdjacencyList(ids []int64, currentID int64) (*int64, *int64) {
	i := slices.Index(ids, currentID)
	if i < 0 {
		return nil, nil
	}
	var prev, next *int64
	if i > 0 {
		p := ids[i-1]
		prev = &p
	}
	if i < len(ids)-1 {
		n := ids[i+1]
		next = &n
	}
	return prev, next
}
