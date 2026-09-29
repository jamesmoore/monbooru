package gallery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
)

type Watcher struct {
	fsw            *fsnotify.Watcher
	galleryName    string
	galleryPath    string
	thumbnailsPath string
	maxFileSizeMB  int
	db             *db.DB
	jobs           *jobs.Manager
	OnEvent        func(msg string)
	OnChange       func()
	OnPhash        PhashSink
	Naming         Naming
	// Read at every event, so a gallery added inside this one is left
	// alone from the moment it exists.
	boundary func() *Boundary

	mu sync.Mutex
	// Paths the watcher renamed itself, until their events have arrived:
	// without it every named file is hashed again by the ingest its own
	// rename triggers.
	selfMoved map[string]time.Time
	closing   bool
	stop      chan struct{}
	timers    map[string]*debounceTimer
	wg        sync.WaitGroup
}

// Each ingest hashes and decodes its file, so a folder moved in would
// otherwise start one per file at once. Vanish checks queue in the same
// line, behind the ingest that may claim their row.
var watcherSlots = make(chan struct{}, 2)

const debounceDelay = 500 * time.Millisecond

// Must outlast the debounced ingest a vanished file may pair with, so a move
// or an in-place replace claims the row before the file is judged gone.
const movedOutGrace = 2 * time.Second

type debounceTimer struct {
	t        *time.Timer
	deadline time.Time
	run      func(string)
}

func NewWatcher(h Handle, maxFileSizeMB int, jobManager *jobs.Manager) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	galleryPath := h.GalleryPath

	w := &Watcher{
		fsw:            fsw,
		galleryName:    h.Name,
		galleryPath:    galleryPath,
		thumbnailsPath: h.ThumbnailsPath,
		maxFileSizeMB:  maxFileSizeMB,
		db:             h.DB,
		jobs:           jobManager,
		boundary:       h.Boundary,
		timers:         map[string]*debounceTimer{},
		selfMoved:      map[string]time.Time{},
		stop:           make(chan struct{}),
	}

	if addErr := fsw.Add(galleryPath); addErr != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("fsnotify watch gallery root: %w", addErr)
	}
	logx.Infof("watcher: watching %s", galleryPath)

	watchCount := 1
	limitHit := false
	if walkErr := WalkTree(w.boundary(), func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == galleryPath {
			return nil
		}
		if limitHit {
			return filepath.SkipAll
		}
		if addErr := fsw.Add(path); addErr != nil {
			// Errno first, as glibc localises the messages; the strings
			// catch wrappers that do not unwrap to syscall.Errno.
			if errors.Is(addErr, syscall.ENOSPC) ||
				errors.Is(addErr, syscall.EMFILE) ||
				strings.Contains(addErr.Error(), "no space left") ||
				strings.Contains(addErr.Error(), "too many open files") {
				logx.Warnf("fsnotify: inotify limit hit at %d dirs. "+
					"Increase: echo fs.inotify.max_user_watches=524288 | sudo tee -a /etc/sysctl.conf && sudo sysctl -p", watchCount)
				limitHit = true
				return filepath.SkipAll
			}
			logx.Warnf("fsnotify add %q: %v", path, addErr)
		} else {
			watchCount++
		}
		return nil
	}); walkErr != nil {
		// The watcher still works for the directories it did register.
		logx.Warnf("watcher: walk %q: %v", galleryPath, walkErr)
	}

	return w, nil
}

func (w *Watcher) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			w.shutdown()
			return nil

		case event, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}

			// A folder fenced off after the watch was taken still reports.
			if w.boundary().Excludes(event.Name) {
				continue
			}

			if w.jobSuppressesIngest() {
				// A folder made meanwhile is still watched; the timers for its
				// files check the job again when they fire.
				if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						w.registerTree(event.Name)
					}
				}
				continue
			}

			if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				info, err := os.Stat(event.Name)
				if err != nil {
					// A Rename carries a move's source; if nothing claims the
					// row within the grace, the file left the library.
					if event.Has(fsnotify.Rename) {
						w.schedule(event.Name, movedOutGrace, w.reconcileVanished)
					}
					continue
				}

				if info.IsDir() {
					w.registerTree(event.Name)
					continue
				}

				w.debounce(event.Name)
			}

			// Slow writers keep firing Write long after Create;
			// debouncing them holds the ingest until the bytes settle.
			if event.Has(fsnotify.Write) {
				w.debounce(event.Name)
			}

			// A delete gets the move's grace: a replace across
			// filesystems deletes and rewrites the path, and marking the
			// row missing in between races the replace's commit.
			if event.Has(fsnotify.Remove) {
				_ = w.fsw.Remove(event.Name)
				w.schedule(event.Name, movedOutGrace, w.reconcileVanished)
			}

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			logx.Warnf("fsnotify error: %v", err)
		}
	}
}

func (w *Watcher) eventPrefix() string {
	if w.galleryName == "" {
		return "watcher: "
	}
	return "watcher [" + w.galleryName + "]: "
}

// Files can land in a new directory before its watch is added, and those
// emit no event, so what is already there is scheduled too.
func (w *Watcher) registerTree(dir string) {
	if !w.linkWorthFollowing(dir) {
		return
	}
	_ = WalkTreeUnder(dir, w.boundary(), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if addErr := w.fsw.Add(path); addErr != nil {
				logx.Warnf("fsnotify add new dir %q: %v", path, addErr)
			}
			return nil
		}
		w.debounce(path)
		return nil
	})
}

// The walk's rule for links, applied here because registerTree starts at
// the link rather than at the gallery root.
func (w *Watcher) linkWorthFollowing(dir string) bool {
	info, err := os.Lstat(dir)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return true
	}
	root, err := filepath.EvalSymlinks(w.galleryPath)
	if err != nil {
		root = filepath.Clean(w.galleryPath)
	}
	target, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	return !PathInside(root, target) && !PathInside(target, root) && !w.boundary().leadsOut(target)
}

// These jobs write image_paths and image_tags in their own transactions; an
// ingest beside them would race their UNIQUE constraints. A timer checks
// again on firing, as it may have been armed before the job started.
func (w *Watcher) jobSuppressesIngest() bool {
	if w.jobs == nil {
		return false
	}
	st := w.jobs.Get()
	if st == nil || !st.Running {
		return false
	}
	// prune-dirs: every directory it unlinks fires a Remove with no row
	// behind it.
	switch st.JobType {
	case "sync", "move", "transfer", "delete", "tag", "prune-dirs":
		return len(st.Galleries) == 0 || slices.Contains(st.Galleries, w.galleryName)
	}
	return false
}

// shutdown waits out fired ingests so the caller can close the DB once
// Run returns.
func (w *Watcher) shutdown() {
	w.mu.Lock()
	w.closing = true
	close(w.stop)
	for path, e := range w.timers {
		e.t.Stop()
		delete(w.timers, path)
	}
	w.mu.Unlock()
	w.wg.Wait()
	_ = w.fsw.Close()
}

func (w *Watcher) debounce(path string) {
	w.schedule(path, debounceDelay, w.ingestFile)
}

// Outlasts both timers a self-rename can arm: the destination's ingest
// and the source's vanish check.
const selfMovedGrace = movedOutGrace + debounceDelay

// Cancelling matters as much as the mark: the rename's events usually arm
// a timer before this runs.
func (w *Watcher) claimSelfMove(paths ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	deadline := time.Now().Add(selfMovedGrace)
	for _, p := range paths {
		if p == "" {
			continue
		}
		if e, ok := w.timers[p]; ok {
			e.t.Stop()
			delete(w.timers, p)
		}
		w.selfMoved[p] = deadline
	}
}

func (w *Watcher) schedule(path string, delay time.Duration, run func(string)) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closing {
		return
	}
	now := time.Now()
	for p, deadline := range w.selfMoved {
		if now.After(deadline) {
			delete(w.selfMoved, p)
		}
	}
	if _, mine := w.selfMoved[path]; mine {
		return
	}
	if e, ok := w.timers[path]; ok {
		e.deadline = time.Now().Add(delay)
		e.run = run
		e.t.Reset(delay)
		return
	}

	e := &debounceTimer{deadline: time.Now().Add(delay), run: run}
	e.t = time.AfterFunc(delay, func() { w.onDebounceFired(path, e) })
	w.timers[path] = e
}

// A Reset that raced this callback moved the deadline on, and that later
// fire takes over. wg.Add runs under the lock so shutdown cannot miss an
// ingest that passed the closing check.
func (w *Watcher) onDebounceFired(path string, e *debounceTimer) {
	w.mu.Lock()
	if w.closing || w.timers[path] != e || time.Now().Before(e.deadline) {
		w.mu.Unlock()
		return
	}
	delete(w.timers, path)
	run := e.run
	w.wg.Add(1)
	w.mu.Unlock()
	defer w.wg.Done()

	if !w.acquireSlot() {
		return
	}
	defer func() { <-watcherSlots }()
	run(path)
}

// acquireSlot is false once the watcher is closing, whether it was
// waiting or had just got a slot.
func (w *Watcher) acquireSlot() bool {
	select {
	case watcherSlots <- struct{}{}:
	case <-w.stop:
		return false
	}
	select {
	case <-w.stop:
		<-watcherSlots
		return false
	default:
		return true
	}
}

func (w *Watcher) reconcileVanished(path string) {
	if w.jobSuppressesIngest() {
		return
	}
	if _, err := os.Stat(path); err == nil {
		return
	}
	w.markFileMissing(path)
}

func (w *Watcher) ingestFile(path string) {
	// Asked again, as the timer may have been armed before the folder was
	// fenced off.
	if w.jobSuppressesIngest() || w.boundary().Excludes(path) {
		return
	}
	if _, err := DetectFileType(path); err != nil {
		return
	}

	if maxMB := w.maxFileSizeMB; maxMB > 0 {
		if info, statErr := os.Stat(path); statErr == nil {
			if info.Size() > int64(maxMB)*1024*1024 {
				logx.Warnf("watcher: skipping %q (size %d exceeds %d MB)",
					path, info.Size(), maxMB)
				return
			}
		}
	}

	img, isDup, err := Ingest(w.db, w.galleryPath, w.thumbnailsPath, path, "")
	if err != nil {
		logx.Warnf("watcher ingest %q: %v", path, err)
	} else if isDup {
		logx.Infof("watcher: duplicate %q", path)
	} else {
		if !w.Naming.Empty() && img != nil {
			path = w.renameIngested(img.ID, path)
		}
		if img != nil {
			w.OnPhash.Stored(img.ID, img.Phash)
		}
		logx.Infof("watcher: ingested %q", path)
		if w.OnEvent != nil {
			w.OnEvent(w.eventPrefix() + "added " + filepath.Base(path))
		}
		if w.OnChange != nil {
			w.OnChange()
		}
	}
}

func (w *Watcher) renameIngested(id int64, path string) string {
	w.claimSelfMove(path)
	// Not cancellable, like the ingest before it: a rename cut short at
	// shutdown would strand the file half-filed.
	newPath, err := w.Naming.Apply(context.Background(), w.db, w.boundary(), id, "", "")
	if err != nil {
		logx.Warnf("watcher: name %q: %v", path, err)
		return path
	}
	if newPath == "" {
		return path
	}
	w.claimSelfMove(newPath)
	return newPath
}

// Without this the image hides until a sync, and Prune missing images
// would drop its row and tags while a copy sits in the tree.
func (w *Watcher) promoteSurvivingCopy(id int64, gonePath string) bool {
	copies, err := AliasPathsFor(w.db.Read, []int64{id})
	if err != nil {
		logx.Warnf("watcher promote copy %d: list paths: %v", id, err)
		return false
	}
	for _, c := range copies[id] {
		p := c.Path
		if w.boundary().Check(p) != nil {
			continue
		}
		info, statErr := os.Stat(p)
		if statErr != nil {
			continue
		}
		// A cheap stand-in for sync's re-hash: a copy overwritten while
		// nothing watched would leave sha256, md5 and file_size
		// describing other bytes.
		if info.Size() != c.Size {
			logx.Warnf("watcher promote copy %d: %q holds %d bytes, not the row's %d - left alone",
				id, p, info.Size(), c.Size)
			continue
		}
		if err := repointCanonical(w.db.Write, id, p, FolderPath(w.galleryPath, p), gonePath); err != nil {
			logx.Warnf("watcher promote copy %d: %v", id, err)
			return false
		}
		logx.Infof("watcher: %q went, kept id=%d at %q", gonePath, id, p)
		if w.OnChange != nil {
			w.OnChange()
		}
		return true
	}
	return false
}

// The raw decrement is deliberate: after is_missing = 1, DropTagUsageTx
// would see a row it no longer counts and refuse.
func (w *Watcher) markFileMissing(path string) {
	rootAbs, err := filepath.Abs(w.galleryPath)
	if err != nil {
		return
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	if !PathInside(rootAbs, pathAbs) {
		return
	}

	var imgID int64
	err = w.db.Read.QueryRow(
		`SELECT id FROM images WHERE canonical_path = ? AND is_missing = 0`, path,
	).Scan(&imgID)
	if err != nil {
		err2 := w.db.Read.QueryRow(
			`SELECT ip.image_id FROM image_paths ip
			 JOIN images i ON i.id = ip.image_id
			 WHERE ip.path = ? AND ip.is_canonical = 1 AND i.is_missing = 0`, path,
		).Scan(&imgID)
		if err2 != nil {
			return
		}
	}

	if w.promoteSurvivingCopy(imgID, path) {
		return
	}

	tx, err := w.db.Write.Begin()
	if err != nil {
		logx.Warnf("watcher mark missing %q: begin tx: %v", path, err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, imgID); err != nil {
		logx.Warnf("watcher mark missing %q: %v", path, err)
		return
	}

	tagIDs, err := db.QueryIDs(tx, `SELECT tag_id FROM image_tags WHERE image_id = ?`, imgID)
	if err != nil {
		logx.Warnf("watcher mark missing %q: list tags: %v", path, err)
		return
	}

	for _, tid := range tagIDs {
		if _, err := tx.Exec(
			`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id = ?`, tid,
		); err != nil {
			logx.Warnf("watcher mark missing %q: decrement tag %d: %v", path, tid, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		logx.Warnf("watcher mark missing %q: commit: %v", path, err)
		return
	}

	logx.Infof("watcher: marked missing %q (id=%d)", path, imgID)
	if w.OnEvent != nil {
		w.OnEvent(w.eventPrefix() + "removed " + filepath.Base(path))
	}
	if w.OnChange != nil {
		w.OnChange()
	}
}
