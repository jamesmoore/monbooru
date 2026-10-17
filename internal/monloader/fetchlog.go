package monloader

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/shared/contract"
)

type FetchEntry struct {
	State  string
	Msg    string
	Hashes string
	At     time.Time
}

// A batch fetch no page polls would otherwise grow the map without bound.
const fetchTTL = 10 * time.Minute

func fetchKey(gallery string, id int64) string {
	return gallery + "\x00" + strconv.FormatInt(id, 10)
}

type FetchLog struct {
	mu sync.Mutex
	m  map[string]FetchEntry
}

func newFetchLog() *FetchLog {
	return &FetchLog{m: map[string]FetchEntry{}}
}

// A terminal report inherits Hashes, which monloader's callback does not
// know; a fresh pending drops them.
func (f *FetchLog) Record(gallery string, id int64, state, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	f.pruneLocked(now)
	key := fetchKey(gallery, id)
	entry := FetchEntry{State: state, Msg: msg, At: now}
	if prev, ok := f.m[key]; ok && state != StatePending {
		entry.Hashes = prev.Hashes
	}
	f.m[key] = entry
}

// Callers set it before the enqueue, or a fast PTR callback could be
// overwritten back to pending.
func (f *FetchLog) RecordLookup(gallery string, id int64, hashes string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	f.pruneLocked(now)
	f.m[fetchKey(gallery, id)] = FetchEntry{State: StatePending, At: now, Hashes: hashes}
}

// The writers prune too, but once the last fetch lands nothing writes again.
func (f *FetchLog) Prune() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked(time.Now())
}

func (f *FetchLog) pruneLocked(now time.Time) {
	maps.DeleteFunc(f.m, func(_ string, e FetchEntry) bool {
		return now.Sub(e.At) > fetchTTL
	})
}

func (f *FetchLog) Load(gallery string, id int64) (FetchEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[fetchKey(gallery, id)]
	return e, ok
}

func (f *FetchLog) Clear(gallery string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, fetchKey(gallery, id))
}

// SplitTrail parts a lookup trail into the PTR's entries and the online
// backends', each in trail order.
func SplitTrail(trail string) (ptr, online []string) {
	for _, entry := range strings.Split(trail, "; ") {
		if strings.HasPrefix(entry, contract.TrailPTR+":") {
			ptr = append(ptr, entry)
		} else {
			online = append(online, entry)
		}
	}
	return ptr, online
}

func SimilarityRan(trail string) bool {
	return slices.ContainsFunc(strings.Split(trail, "; "), func(entry string) bool {
		name, reason, ok := strings.Cut(entry, ": ")
		return ok && (name == contract.TrailIQDB || name == contract.TrailSauceNAO) && !strings.HasPrefix(reason, "skipped")
	})
}
