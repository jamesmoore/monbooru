// Package library owns one open gallery: its database, services, caches
// and background work.
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

type Gallery struct {
	gallery.Handle

	RelationsSvc *relations.Service
	Degraded     bool

	// Resolved once at open: the built-in row never changes.
	GeneralCategoryID int64

	folderTree        atomic.Pointer[[]gallery.FolderNode]
	sourceLabelCounts atomic.Pointer[[]gallery.SourceLabelCount]

	// Copy-on-write maps: readers hold no lock, so a stored map is never
	// mutated.
	inboxCountUnder        atomic.Pointer[map[string]int]
	phashMissingUnder      atomic.Pointer[map[string]int]
	folderTreeUnder        atomic.Pointer[map[string][]gallery.FolderNode]
	sourceLabelCountsUnder atomic.Pointer[map[string][]gallery.SourceLabelCount]

	BKTree *relations.BKTree

	watcherCancel context.CancelFunc
	watcherDone   chan struct{}

	mangaReclaim *gallery.MangaCacheReclaimer

	exports atomic.Int32
}

// Sync drops the caches itself, so no caller has to remember InvalidateCaches.
func (cx *Gallery) Sync(ctx context.Context, maxFileSizeMB int, naming gallery.Naming, progress func(processed, total int, message string)) (gallery.SyncResult, error) {
	result, err := gallery.Sync(ctx, cx.DB, cx.Boundary(), cx.ThumbnailsPath, maxFileSizeMB, naming, progress, cx.InvalidateCaches, relations.PhashSink(cx.DB))
	cx.InvalidateCaches()
	return result, err
}

// InvalidateCaches must follow every write that changes image membership.
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
	search.AdjacencyCacheDropForGallery(cx.Name)
}

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

func (cx *Gallery) FolderTree() ([]gallery.FolderNode, error) {
	return cachedValue(&cx.folderTree, func() ([]gallery.FolderNode, error) { return gallery.FolderTree(cx.DB) })
}

func (cx *Gallery) SourceLabelCounts() ([]gallery.SourceLabelCount, error) {
	return cachedValue(&cx.sourceLabelCounts, func() ([]gallery.SourceLabelCount, error) { return gallery.SourceLabelCountsQuery(cx.DB, 25) })
}

func (cx *Gallery) VisibleCount() (int, bool) { return counts.VisibleCount(cx.DB) }
func (cx *Gallery) InboxCount() (int, bool)   { return counts.InboxCount(cx.DB) }
func (cx *Gallery) TagCount() (int, bool)     { return counts.TagCount(cx.DB) }

func (cx *Gallery) CollectionsCount() (int, bool) { return counts.CollectionsCount(cx.DB) }

func okErr(n int, ok bool) (int, error) {
	if !ok {
		return 0, errCountUnavailable
	}
	return n, nil
}

var errCountUnavailable = errors.New("count unavailable")

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

func (cx *Gallery) InboxCountUnder(c *Ceiling) (int, error) {
	return ceilingCached(c, func() (int, error) { return okErr(cx.InboxCount()) }, &cx.inboxCountUnder,
		func() (int, error) { return gallery.InboxCountUnder(cx.DB, c.ExcludedTagIDs()) })
}

func (cx *Gallery) PhashMissingUnder(c *Ceiling) (int, error) {
	return ceilingCached(c,
		func() (int, error) { return okErr(counts.PhashMissing(cx.DB)) },
		&cx.phashMissingUnder,
		func() (int, error) { return gallery.PhashMissingUnder(cx.DB, c.ExcludedTagIDs()) })
}

// InvalidatePhashMissing is for phash writes that leave image membership alone.
func (cx *Gallery) InvalidatePhashMissing() {
	if cx == nil {
		return
	}
	counts.InvalidatePhashMissing(cx.DB)
	cx.phashMissingUnder.Store(nil)
}

func (cx *Gallery) FolderTreeUnder(c *Ceiling) ([]gallery.FolderNode, error) {
	return ceilingCached(c, cx.FolderTree, &cx.folderTreeUnder,
		func() ([]gallery.FolderNode, error) { return gallery.FolderTreeUnder(cx.DB, c.ExcludedTagIDs()) })
}

func (cx *Gallery) SourceLabelCountsUnder(c *Ceiling) ([]gallery.SourceLabelCount, error) {
	return ceilingCached(c, cx.SourceLabelCounts, &cx.sourceLabelCountsUnder,
		func() ([]gallery.SourceLabelCount, error) {
			return gallery.SourceLabelCountsUnderQuery(cx.DB, 25, c.ExcludedTagIDs())
		})
}

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
			Bounds:         new(atomic.Pointer[gallery.Boundary]),
		},
		RelationsSvc:      relations.New(database),
		Degraded:          degraded,
		GeneralCategoryID: generalID,
		BKTree:            tree,
	}, nil
}

func (cx *Gallery) MangaCacheDir() string {
	if cx == nil {
		return ""
	}
	return gallery.MangaCacheDir(cx.ThumbnailsPath)
}

// Close leaves cx.DB set: an unjoined WarmCaches may still read it, and a
// closed pool errors where a nil one would panic.
func (cx *Gallery) Close() {
	cx.stopWatcher()
	cx.stopMangaReclaim()
	// Keyed by name, the cache would outlive this database and hand its
	// ids to the next library opened under the name.
	search.AdjacencyCacheDropForGallery(cx.Name)
	if cx.DB != nil {
		relations.DefaultRegistry.Unregister(cx.DB)
		counts.Release(cx.DB)
		_ = cx.DB.Close()
	}
}

// An export streams without the gallery lock, so whatever closes the
// gallery must check Exporting first.
func (cx *Gallery) BeginExport()    { cx.exports.Add(1) }
func (cx *Gallery) EndExport()      { cx.exports.Add(-1) }
func (cx *Gallery) Exporting() bool { return cx.exports.Load() > 0 }

// WatcherRunning and ReclaimerRunning exist for other packages' tests.
func (cx *Gallery) WatcherRunning() bool   { return cx != nil && cx.watcherCancel != nil }
func (cx *Gallery) ReclaimerRunning() bool { return cx != nil && cx.mangaReclaim != nil }

func (cx *Gallery) StartBackground(watchEnabled bool, maxFileSizeMB int, naming gallery.Naming, jm *jobs.Manager) {
	cx.startWatcher(watchEnabled, maxFileSizeMB, naming, jm)
	cx.startMangaReclaim()
	cx.indexWorkflows()
}

// Outside the job slot: that slot is single and scoped to the active
// gallery, and every opened gallery runs this.
func (cx *Gallery) indexWorkflows() {
	if cx.DB == nil {
		return
	}
	pending, err := gallery.ComfyTermsPending(cx.DB)
	if err != nil || pending == 0 {
		return
	}
	go func() {
		indexed, err := gallery.BackfillComfyTerms(context.Background(), cx.DB, nil)
		if indexed > 0 {
			cx.InvalidateCaches()
		}
		if err != nil {
			logx.Warnf("gallery %q: workflow index: %v", cx.Name, err)
			return
		}
		logx.Infof("gallery %q: indexed %d workflow(s)", cx.Name, indexed)
	}()
}

func (cx *Gallery) startWatcher(watchEnabled bool, maxFileSizeMB int, naming gallery.Naming, jm *jobs.Manager) {
	if !watchEnabled || cx.Degraded || cx.watcherCancel != nil {
		return
	}
	w, err := gallery.NewWatcher(cx.Handle, maxFileSizeMB, jm)
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

// RestartWatcher is for a folder handed back; the event filter already
// follows a folder fenced off.
func (cx *Gallery) RestartWatcher(watchEnabled bool, maxFileSizeMB int, naming gallery.Naming, jm *jobs.Manager) {
	cx.stopWatcher()
	cx.startWatcher(watchEnabled, maxFileSizeMB, naming, jm)
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
