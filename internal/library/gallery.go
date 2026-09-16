// Package library owns one open gallery: its paths, its database, the
// services and caches that hang off them, and the background work that
// keeps them fresh. It is the application's per-gallery aggregate.
//
// It sits above internal/gallery and internal/relations rather than
// inside either, because it holds both and they cannot hold each other.
// It sits outside internal/web because a gallery is not a transport
// concern: the HTTP package holds a pointer to one and every handler
// reaches its state through that, which is the arrangement the three
// process-global registries below it exist to work around.
package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tags"
)

// Gallery holds everything per-gallery: paths, DB, tag service, degraded
// flag, and watcher bookkeeping.
type Gallery struct {
	// The five fields both transports describe identically. Embedded so a
	// reader still writes cx.DB / cx.GalleryPath, and so the API resolver
	// hands the same value across instead of rebuilding it field by field.
	gallery.Handle

	RelationsSvc *relations.Service
	Degraded     bool

	// GeneralCategoryID is the resolved id of the built-in `general`
	// tag_categories row. parseTagInput hits this every detail-page tag
	// add, every upload, every batch-tag job; resolving once at open
	// time saves a SELECT per call. Built-in rows are immutable so no
	// invalidation is needed.
	GeneralCategoryID int64

	// Per-gallery caches of queries that scan every visible image. All are
	// nilled by InvalidateCaches after any ingest/delete/missing-toggle so
	// the next reader re-populates from SQLite. The int counts are pointers
	// so "not cached" is distinguishable from "cached zero".
	folderTree        atomic.Pointer[[]gallery.FolderNode]
	sourceLabelCounts atomic.Pointer[[]gallery.SourceLabelCount]

	// Parallel caches keyed by ceiling level, populated lazily on first
	// access from a sidebar / relations-hub render under that ceiling
	// and dropped together with the blind caches on InvalidateCaches.
	// The maps are stored by atomic pointer so reads are lock-free; the
	// helper below performs copy-on-write to add a level. Three active
	// ceilings × N galleries at steady state is trivial storage.
	inboxCountUnder        atomic.Pointer[map[string]int]
	phashMissingUnder      atomic.Pointer[map[string]int]
	folderTreeUnder        atomic.Pointer[map[string][]gallery.FolderNode]
	sourceLabelCountsUnder atomic.Pointer[map[string][]gallery.SourceLabelCount]

	// BKTree is the in-memory phash index used by the find-pairs job
	// and the phash: search keyword. Built lazily on first relations
	// query; the ingest/delete hooks in internal/relations keep it
	// consistent with subsequent writes once it is built.
	BKTree *relations.BKTree

	watcherCancel context.CancelFunc
	watcherDone   chan struct{}

	mangaReclaim *gallery.MangaCacheReclaimer
}

// Sync runs gallery.Sync against this context and drops the per-cx
// caches that the sync's mark-missing / move / ingest steps touch.
// Centralising the invalidation here keeps the contract local: every
// caller (manual sync handler, scheduler, future scheduled phase) gets
// the cache hygiene by construction instead of relying on the caller's
// goroutine to remember the InvalidateCaches at the right point.
func (cx *Gallery) Sync(ctx context.Context, maxFileSizeMB int, naming gallery.Naming, progress func(processed, total int, message string)) (gallery.SyncResult, error) {
	result, err := gallery.Sync(ctx, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, maxFileSizeMB, naming, progress, relations.PhashSink(cx.DB))
	cx.InvalidateCaches()
	return result, err
}

// InvalidateCaches drops the folder-tree and visible-count caches. Call after
// any mutation that changes which images are visible (ingest, delete, sync
// mark-missing, watcher remove). Tag and collection counts are dropped too:
// the same image-mutation paths typically change those, and the Settings
// page's per-gallery cells need them fresh.
func (cx *Gallery) InvalidateCaches() {
	if cx == nil {
		return
	}
	cx.folderTree.Store(nil)
	cx.sourceLabelCounts.Store(nil)
	cx.inboxCountUnder.Store(nil)
	cx.phashMissingUnder.Store(nil)
	cx.folderTreeUnder.Store(nil)
	cx.sourceLabelCountsUnder.Store(nil)
	if cx.DB != nil {
		counts.Invalidate(cx.DB)
	}
	// The adjacency cache holds sorted match-id snapshots that pre-date
	// any membership-changing write (delete, move, inbox/favourite
	// toggle, batch tag). Without dropping them here, a re-render of
	// the same query within the 5-min TTL serves the stale list and
	// the gallery shows rows that no longer match.
	search.AdjacencyCacheDropForGallery(cx.Name)
}

// cachedValue lazy-loads and caches a value: the atomic pointer doubles
// as the cache slot and the "loaded?" flag, and nil means re-query. The
// scalar tallies use counts.cachedCount, which is the same shape.
func cachedValue[V any](slot *atomic.Pointer[V], query func() (V, error)) (V, error) {
	if p := slot.Load(); p != nil {
		return *p, nil
	}
	v, err := query()
	if err != nil {
		var zero V
		return zero, err
	}
	slot.Store(&v)
	return v, nil
}

// FolderTree returns the cached tree or builds one on demand. The cache is
// invalidated by InvalidateCaches.
func (cx *Gallery) FolderTree() ([]gallery.FolderNode, error) {
	return cachedValue(&cx.folderTree, func() ([]gallery.FolderNode, error) { return gallery.FolderTree(cx.DB) })
}

// SourceLabelCounts returns the cached top-25 source labels for the
// gallery's non-missing image rows. Empty when no image carries a
// source label; the sidebar partial gates rendering on the slice
// being non-empty.
func (cx *Gallery) SourceLabelCounts() ([]gallery.SourceLabelCount, error) {
	return cachedValue(&cx.sourceLabelCounts, func() ([]gallery.SourceLabelCount, error) { return gallery.SourceLabelCountsQuery(cx.DB, 25) })
}

// The whole-library tallies live in internal/counts, which keys them by
// *db.DB and drops them from InvalidateCaches above.
func (cx *Gallery) VisibleCount() (int, bool) { return counts.VisibleCount(cx.DB) }
func (cx *Gallery) InboxCount() (int, bool)   { return counts.InboxCount(cx.DB) }
func (cx *Gallery) TagCount() (int, bool)     { return counts.TagCount(cx.DB) }

func (cx *Gallery) CollectionsCount() (int, bool) { return counts.CollectionsCount(cx.DB) }

// okErr adapts the counts package's (value, ok) shape to the error shape
// the ceiling helpers take.
func okErr(n int, ok bool) (int, error) {
	if !ok {
		return 0, errCountUnavailable
	}
	return n, nil
}

var errCountUnavailable = errors.New("count unavailable")

// lookupByCeiling reads a per-ceiling cache slot; returns (zero, false)
// when the level isn't yet cached. Copy-on-write semantics: the caller
// stages a new map and Stores it on a miss so concurrent readers see a
// fully-formed snapshot.
func lookupByCeiling[V any](slot *atomic.Pointer[map[string]V], level string) (V, bool) {
	if m := slot.Load(); m != nil {
		if v, ok := (*m)[level]; ok {
			return v, true
		}
	}
	var zero V
	return zero, false
}

func storeByCeiling[V any](slot *atomic.Pointer[map[string]V], level string, value V) {
	for {
		current := slot.Load()
		newMap := make(map[string]V, 4)
		if current != nil {
			for k, v := range *current {
				newMap[k] = v
			}
		}
		newMap[level] = value
		if slot.CompareAndSwap(current, &newMap) {
			return
		}
	}
}

// ceilingCached routes cx.XUnder accessors through the shared "inactive
// ceiling delegates to blind / cache / query / store" shape.
func ceilingCached[V any](c *Ceiling, blind func() (V, error), slot *atomic.Pointer[map[string]V], query func() (V, error)) (V, error) {
	if c == nil || !c.IsActive() {
		return blind()
	}
	if v, ok := lookupByCeiling(slot, c.level); ok {
		return v, nil
	}
	v, err := query()
	if err != nil {
		var zero V
		return zero, err
	}
	storeByCeiling(slot, c.level, v)
	return v, nil
}

// InboxCountUnder returns the count of visible inbox images excluding any
// whose tag list intersects the ceiling's excluded rating ids. An inactive
// ceiling delegates to the blind InboxCount.
func (cx *Gallery) InboxCountUnder(c *Ceiling) (int, error) {
	return ceilingCached(c, func() (int, error) { return okErr(cx.InboxCount()) }, &cx.inboxCountUnder,
		func() (int, error) { return gallery.InboxCountUnder(cx.DB, c.ExcludedTagIDs()) })
}

// PhashMissingUnder returns the relations-hub "PhashMissing" count
// excluding rows above the ceiling. An inactive ceiling reads from
// the blind cache; loadRelationsCounts is invoked on every relations
// hub / browse render, and the underlying SELECT walks every visible
// row because the phash partial index excludes NULLs. The cache is
// dropped on any ingest / delete (InvalidateCaches) and on every
// phash write (InvalidatePhashMissing).
func (cx *Gallery) PhashMissingUnder(c *Ceiling) (int, error) {
	return ceilingCached(c,
		func() (int, error) { return okErr(counts.PhashMissing(cx.DB)) },
		&cx.phashMissingUnder,
		func() (int, error) { return gallery.PhashMissingUnder(cx.DB, c.ExcludedTagIDs()) })
}

// InvalidatePhashMissing drops the cached PhashMissing counts. Call
// after a phash write that changes the NULL/non-NULL count: single-
// image recompute, the compute-phashes backfill, rebuild-thumbnails
// completion. Ingest / delete already route through InvalidateCaches.
func (cx *Gallery) InvalidatePhashMissing() {
	if cx == nil {
		return
	}
	counts.InvalidatePhashMissing(cx.DB)
	cx.phashMissingUnder.Store(nil)
}

// FolderTreeUnder returns the ceiling-aware folder tree. An inactive
// ceiling delegates to the blind FolderTree cache.
func (cx *Gallery) FolderTreeUnder(c *Ceiling) ([]gallery.FolderNode, error) {
	return ceilingCached(c, cx.FolderTree, &cx.folderTreeUnder,
		func() ([]gallery.FolderNode, error) { return gallery.FolderTreeUnder(cx.DB, c.ExcludedTagIDs()) })
}

// SourceLabelCountsUnder returns the ceiling-aware top-25 source labels.
func (cx *Gallery) SourceLabelCountsUnder(c *Ceiling) ([]gallery.SourceLabelCount, error) {
	return ceilingCached(c, cx.SourceLabelCounts, &cx.sourceLabelCountsUnder,
		func() ([]gallery.SourceLabelCount, error) {
			return gallery.SourceLabelCountsUnderQuery(cx.DB, 25, c.ExcludedTagIDs())
		})
}

// WarmCaches primes the per-gallery aggregations so the first user-facing
// sidebar/gallery/settings request doesn't pay the cold scan. Errors are
// ignored: the lazy path in each accessor still recomputes on demand if the
// warm failed.
func (cx *Gallery) WarmCaches() {
	if cx == nil || cx.DB == nil {
		return
	}
	cx.FolderTree()        //nolint:errcheck
	cx.SourceLabelCounts() //nolint:errcheck
	cx.VisibleCount()
	cx.InboxCount()
	cx.TagCount()
	cx.CollectionsCount()
}

// Open opens the DB and creates the thumbnails directory. The
// watcher is started separately so only the active gallery runs one.
func Open(g config.Gallery) (*Gallery, error) {
	if dbDir := filepath.Dir(g.DBPath); dbDir != "" && dbDir != "." {
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			return nil, fmt.Errorf("gallery %q: create db dir: %w", g.Name, err)
		}
	}
	database, err := db.Open(g.DBPath)
	if err != nil {
		return nil, fmt.Errorf("gallery %q: open db: %w", g.Name, err)
	}
	if err := db.Bootstrap(database); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("gallery %q: bootstrap db: %w", g.Name, err)
	}
	if err := os.MkdirAll(g.ThumbnailsPath, 0o755); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("gallery %q: create thumbnails dir: %w", g.Name, err)
	}
	degraded := false
	if _, err := os.ReadDir(g.GalleryPath); err != nil {
		logx.Warnf("gallery %q: path %q unreadable: %v - degraded mode", g.Name, g.GalleryPath, err)
		degraded = true
	}
	var generalID int64
	if err := database.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = 'general'`,
	).Scan(&generalID); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("gallery %q: resolve general category: %w", g.Name, err)
	}
	tree := relations.NewBKTree()
	relations.DefaultRegistry.Register(database, tree)
	return &Gallery{
		Handle: gallery.Handle{
			Name:           g.Name,
			GalleryPath:    g.GalleryPath,
			ThumbnailsPath: g.ThumbnailsPath,
			DBPath:         g.DBPath,
			DB:             database,
			TagSvc:         tags.New(database),
		},
		RelationsSvc:      relations.New(database),
		Degraded:          degraded,
		GeneralCategoryID: generalID,
		BKTree:            tree,
	}, nil
}

// MangaCacheDir returns the per-gallery manga page cache directory.
// Sibling to ThumbnailsPath under the gallery's data root.
func (cx *Gallery) MangaCacheDir() string {
	if cx == nil {
		return ""
	}
	return gallery.MangaCacheDir(cx.ThumbnailsPath)
}

// Close stops the watcher and closes the DB. Keeps cx.DB non-nil afterwards:
// a concurrent WarmCaches goroutine (spawned by StartWatchers, not joined)
// can still race against Close at shutdown or gallery removal. A closed pool
// returns "database is closed" on subsequent calls, which the accessors
// discard; a nil pool would panic on the field deref. sql.DB.Close is
// idempotent so a later Close still behaves.
func (cx *Gallery) Close() {
	cx.stopWatcher()
	cx.stopMangaReclaim()
	// The adjacency cache is keyed by gallery name, so an entry outlives the
	// database it was read from unless it goes when that database does: an
	// import, a repoint or a re-added name would serve the previous
	// library's ids, and executeFromCachedIDs re-reads them by primary key
	// without the query's WHERE.
	search.AdjacencyCacheDropForGallery(cx.Name)
	if cx.DB != nil {
		relations.DefaultRegistry.Unregister(cx.DB)
		counts.Release(cx.DB)
		_ = cx.DB.Close()
	}
}

// WatcherRunning and ReclaimerRunning report whether the two background
// halves are up. The fields behind them stay unexported - the lifecycle is
// this package's - and a caller that reopens a gallery has to be able to
// say the work came back with it.
func (cx *Gallery) WatcherRunning() bool   { return cx != nil && cx.watcherCancel != nil }
func (cx *Gallery) ReclaimerRunning() bool { return cx != nil && cx.mangaReclaim != nil }

// StartBackground starts everything a freshly-opened gallery runs in the
// background. Paired because a gallery given only the watcher never
// evicts the pages its reader extracts, for the life of the process.
func (cx *Gallery) StartBackground(watchEnabled bool, maxFileSizeMB int, naming gallery.Naming, jm *jobs.Manager) {
	cx.startWatcher(watchEnabled, maxFileSizeMB, naming, jm)
	cx.startMangaReclaim()
}

// startWatcher no-ops when watching is disabled, the gallery is degraded,
// or a watcher is already running.
func (cx *Gallery) startWatcher(watchEnabled bool, maxFileSizeMB int, naming gallery.Naming, jm *jobs.Manager) {
	if !watchEnabled || cx.Degraded || cx.watcherCancel != nil {
		return
	}
	w, err := gallery.NewWatcher(cx.Name, cx.GalleryPath, cx.ThumbnailsPath, maxFileSizeMB, cx.DB, jm)
	if err != nil {
		logx.Warnf("gallery %q: watcher start: %v", cx.Name, err)
		return
	}
	w.Naming = naming
	w.OnEvent = jm.SetWatcherMessage
	w.OnChange = cx.InvalidateCaches
	w.OnPhash = relations.PhashSink(cx.DB)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	cx.watcherCancel = cancel
	cx.watcherDone = done
	go func() {
		defer close(done)
		if err := w.Run(ctx); err != nil {
			logx.Warnf("gallery %q: watcher stopped: %v", cx.Name, err)
		}
	}()
	logx.Infof("gallery %q: watcher started", cx.Name)
}

func (cx *Gallery) stopWatcher() {
	if cx.watcherCancel == nil {
		return
	}
	cx.watcherCancel()
	<-cx.watcherDone
	cx.watcherCancel = nil
	cx.watcherDone = nil
}

// startMangaReclaim spawns the per-gallery idle-page evictor. Idempotent
// against repeated calls. The reclaimer is harmless on galleries that
// have never ingested a manga - sweepOnce no-ops when the cache dir
// doesn't exist.
func (cx *Gallery) startMangaReclaim() {
	if cx.mangaReclaim != nil {
		return
	}
	r := gallery.NewMangaCacheReclaimer(cx.MangaCacheDir())
	r.Start(context.Background())
	cx.mangaReclaim = r
}

func (cx *Gallery) stopMangaReclaim() {
	if cx.mangaReclaim == nil {
		return
	}
	cx.mangaReclaim.Stop()
	cx.mangaReclaim = nil
}
