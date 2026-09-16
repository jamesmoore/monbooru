// Package web is the HTTP transport: the mux, the handlers, the templates
// they render and the session, CSRF and rating-cookie plumbing around
// them. It owns how a request becomes a response and nothing about what a
// gallery is - that is internal/library, which this holds a pointer to and
// every handler reaches its state through.
//
// The two things that still live here and read as though they should not:
// the daily scheduler, because the loop needs the config lock and the job
// manager as much as it needs the galleries; and the peer surfaces for
// monloader and plugins, whose panels and receipts are transport but whose
// catalog reconciliation is the tag domain. Moving either one means
// threading the aggregate through the packages below it first.
package web

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/api"
	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/desktop"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/plugins"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
	webFS "github.com/monbooru/monbooru/web"
)

// groupOrdered buckets items by key in first-appearance order. skip drops
// an item entirely (nil keeps all); newGroup builds a bucket from its first
// item; add appends the item to its bucket.
func groupOrdered[T, G any](items []T, skip func(T) bool, key func(T) string, newGroup func(T) *G, add func(*G, T)) []G {
	order := []string{}
	groups := map[string]*G{}
	for _, t := range items {
		if skip != nil && skip(t) {
			continue
		}
		k := key(t)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
			groups[k] = newGroup(t)
		}
		add(groups[k], t)
	}
	out := make([]G, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	return out
}

// tagGroup is used by the groupByCategory template function.
type tagGroup struct {
	Name  string
	Color string
	Tags  []models.Tag
}

// Server holds all shared state for the HTTP server.
type Server struct {
	cfg        *config.Config
	configPath string
	cfgMu      sync.RWMutex // guards cfg reads/writes and config.Save calls
	jobs       *jobs.Manager
	pairs      *pairStore
	sessions   *SessionStore
	loginRL    *loginRateLimiter
	csrfSecret []byte // per-instance HMAC key for CSRF tokens
	tmpl       *template.Template
	staticFS   fs.FS
	done       chan struct{} // closed on Close() to stop background goroutines

	// desktop marks the -desktop profile. It gates the controls that only
	// make sense on the machine the operator is sitting at: the directory
	// picker, the folder opener, Quit, and the first-run redirect.
	desktop bool
	// folderOpener hands a folder to the platform opener. A field so the
	// handler runs where none is installed and pops no window.
	folderOpener func(string) error
	// logDir is where the profile put the log file. Only the command knows:
	// it resolves the layout before the config is loaded.
	logDir string
	// quit carries a Quit click to the command's shutdown select, so the
	// stop path is the same one SIGTERM takes. quitOnce keeps a second
	// click from closing it twice. A Restart click rides the same path with
	// the flag set, and the command starts a fresh process once it has
	// drained.
	quit     chan struct{}
	quitOnce sync.Once
	restart  atomic.Bool

	// ctxMu serialises the gallery mutations and is held read-locked for the
	// length of a gallery-read request, so a swap cannot land mid-render. The
	// state itself is read through the atomic pointer, never through the
	// lock: a handler re-entering ctxMu while its registration holds it
	// read-locked would block behind a pending writer, and that writer waits
	// on the read lock the handler is inside.
	ctxMu    sync.RWMutex
	galState atomic.Pointer[galleryState]

	sched *scheduler

	mlStatus *monloader.StatusCache

	themeWarn *themeWarnings

	peers *plugins.Peers

	fetchStatus *fetchStatusStore
}

// Desktop is what the -desktop profile tells the server: whether it is
// active, and where it put the log file. The second is not derivable here
// because the command resolves the layout before the config is loaded, and
// a config that moved data_path by hand would send the Logs button
// somewhere the log never was.
type Desktop struct {
	Active bool
	LogDir string
}

// NewServer creates the HTTP server with all routes wired. One *db.DB is
// opened per configured gallery.
func NewServer(cfg *config.Config, configPath string, jobManager *jobs.Manager, dk Desktop) (*Server, error) {
	sessions := NewSessionStore()

	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(webFS.FS, "templates/*.html", "templates/partials/*.html")
	if err != nil {
		return nil, err
	}

	staticFS, err := fs.Sub(webFS.FS, "static")
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:          cfg,
		configPath:   configPath,
		jobs:         jobManager,
		pairs:        newPairStore(),
		sessions:     sessions,
		loginRL:      newLoginRateLimiter(),
		csrfSecret:   mustRandBytes(32),
		tmpl:         tmpl,
		staticFS:     staticFS,
		done:         make(chan struct{}),
		desktop:      dk.Active,
		folderOpener: desktop.OpenFolder,
		logDir:       dk.LogDir,
		quit:         make(chan struct{}),
		sched:        newScheduler(),
		mlStatus:     &monloader.StatusCache{},
		fetchStatus:  newFetchStatusStore(),
		themeWarn:    &themeWarnings{},
	}
	s.peers = plugins.NewPeers(s.pluginCallbackURL, s.done)

	applyRelationsConfig(cfg.Relations)

	opened := &galleryState{contexts: map[string]*galleryCtx{}, active: cfg.DefaultGallery}
	for _, g := range cfg.Galleries {
		cx, err := library.Open(g)
		if err != nil {
			for _, done := range opened.contexts {
				done.Close()
			}
			return nil, err
		}
		opened.contexts[g.Name] = cx
	}
	s.galState.Store(opened)

	if cfg.Server.ThemeColor != "" && !tags.IsValidCategoryColor(cfg.Server.ThemeColor) {
		logx.Warnf("server.theme_color %q is not a #rgb / #rrggbb colour; the bundled palette is used", cfg.Server.ThemeColor)
		s.cfg.Server.ThemeColor = ""
	}

	// Periodically sweep expired sessions and login rate-limiter entries.
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sessions.SweepExpired()
				s.loginRL.sweep()
			case <-s.done:
				return
			}
		}
	}()

	go s.runMemoryReclaim()

	// Daily scheduled maintenance runs driven by cfg.Schedule.
	go s.runScheduler()

	go s.runPluginProbes()
	s.seedThemesDir()
	s.ensurePluginsDir()
	s.startManagedPlugins()

	return s, nil
}

// idleIndexReleaseAfter is how long a gallery's phash and counted-tag
// indexes may sit unread before the reclaim loop drops them. Long
// enough that a browse session pausing doesn't pay the rebuild, short
// enough that a finished one gives the memory back.
const idleIndexReleaseAfter = 30 * time.Minute

// reclaimTick is how often the reclaim loop looks at the process,
// reclaimInterval how often it reclaims while nothing else happens. The
// two differ so a job's peak working set - the largest this process ever
// holds - is handed back when the job ends instead of staying resident
// until the interval comes round.
const (
	reclaimTick     = 30 * time.Second
	reclaimInterval = 5 * time.Minute
)

// reclaimDue reports whether an idle tick owes a reclaim: a job ended
// since the last one, or the interval has elapsed.
func reclaimDue(jobEnded bool, since time.Duration) bool { return jobEnded || since >= reclaimInterval }

// runMemoryReclaim wakes every reclaimTick and, when no job is active,
// drops each gallery's idle in-memory indexes, shrinks its SQLite page
// cache, returns the Go heap, and tears down the cached auto-tagger
// session set if it has been idle for tagger.idle_release_after_minutes.
func (s *Server) runMemoryReclaim() {
	ticker := time.NewTicker(reclaimTick)
	defer ticker.Stop()
	last := time.Now()
	seen := s.jobs.Finished()
	for {
		select {
		case <-ticker.C:
			if s.jobs.IsRunning() {
				continue
			}
			ended := s.jobs.Finished()
			if !reclaimDue(ended != seen, time.Since(last)) {
				continue
			}
			seen, last = ended, time.Now()
			ctxs := s.allContexts()
			for _, cx := range ctxs {
				dropped := counts.ReleaseIdleCountedTags(cx.DB, idleIndexReleaseAfter)
				if cx.BKTree != nil && cx.BKTree.ReleaseIdle(idleIndexReleaseAfter) {
					dropped = true
				}
				if dropped {
					logx.Debugf("memory reclaim %q: dropped idle indexes", cx.Name)
				}
				if err := cx.DB.ShrinkMemory(context.Background()); err != nil {
					logx.Warnf("memory reclaim %q: %v", cx.Name, err)
				}
			}
			search.AdjacencyCacheSweep()
			s.fetchStatus.prune()
			s.reconcileAllLookups()
			debug.FreeOSMemory()
			s.cfgMu.Lock()
			mins := s.cfg.Tagger.IdleReleaseAfterMinutes
			s.cfgMu.Unlock()
			if mins > 0 {
				before := readVmRSS()
				if tagger.ReleaseIdle(time.Duration(mins) * time.Minute) {
					after := readVmRSS()
					if before > 0 && after > 0 && before > after {
						logx.Infof("memory reclaim: released idle auto-tagger session (-%s)", humanBytesFmt(int64(before-after)))
					} else {
						logx.Infof("memory reclaim: released idle auto-tagger session")
					}
				}
			}
		case <-s.done:
			return
		}
	}
}

// galleryState is the set of open galleries and the name of the active one,
// published as one immutable value so every reader is a single atomic load.
// Writers hold ctxMu, copy, mutate the copy and store it.
type galleryState struct {
	contexts map[string]*galleryCtx
	active   string
}

// clone returns a copy for a writer to mutate before publishing.
func (st *galleryState) clone() *galleryState {
	next := &galleryState{contexts: make(map[string]*galleryCtx, len(st.contexts)+1), active: st.active}
	maps.Copy(next.contexts, st.contexts)
	return next
}

// galleryState returns the published gallery state.
func (s *Server) galleryState() *galleryState { return s.galState.Load() }

// activeGallery names the runtime-active gallery.
func (s *Server) activeGallery() string { return s.galleryState().active }

// active returns the currently-active gallery context.
func (s *Server) active() *galleryCtx {
	st := s.galleryState()
	return st.contexts[st.active]
}

// get returns the gallery context with the given name, or nil.
func (s *Server) get(name string) *galleryCtx { return s.galleryState().contexts[name] }

// routeMode is what a route says about ctxMu, the lock that keeps a gallery
// swap from landing in the middle of a request.
type routeMode int

const (
	// modeRead holds ctxMu read-locked around the handler, so the gallery it
	// resolves cannot be closed under it. The default, and what any route
	// that renders or queries a gallery wants.
	modeRead routeMode = iota
	// modeWrite takes no lock here: the handler takes ctxMu.Lock itself, and
	// deadlocks against a read the registration had already taken.
	modeWrite
	// modeFree takes no lock and promises not to hold a gallery context
	// across the unlocked window - either it reads no gallery at all, or its
	// slow part is an outbound call that a held read lock would stall a
	// gallery switch behind.
	modeFree
)

// routes is the only way to reach the mux: registerRoutes never names it, so
// a route has to pick a mode to compile at all.
type routes struct {
	s     *Server
	mux   *http.ServeMux
	modes map[string]routeMode
}

func (s *Server) newRoutes() *routes {
	return &routes{s: s, mux: http.NewServeMux(), modes: map[string]routeMode{}}
}

func (rt *routes) add(pattern string, mode routeMode, h http.HandlerFunc) {
	rt.modes[pattern] = mode
	rt.mux.HandleFunc(pattern, h)
}

func (rt *routes) read(pattern string, h http.HandlerFunc) {
	s := rt.s
	rt.add(pattern, modeRead, func(w http.ResponseWriter, r *http.Request) {
		s.ctxMu.RLock()
		defer s.ctxMu.RUnlock()
		h(w, r)
	})
}

func (rt *routes) write(pattern string, h http.HandlerFunc) { rt.add(pattern, modeWrite, h) }

func (rt *routes) free(pattern string, h http.HandlerFunc) { rt.add(pattern, modeFree, h) }

// StartWatchers starts a watcher on every configured gallery at startup. Each
// gallery owns its own watcher for the lifetime of the process so file drops
// into any gallery are picked up in real time, not just the active one.
//
// Also spawns a pre-warm goroutine per gallery that populates the FolderTree,
// source-label, and visible-count caches. The first user request then hits
// warm caches instead of paying a cold aggregation scan against every
// visible image - on libraries with tens of thousands of images that walk
// was the dominant contributor to first-sidebar latency.
func (s *Server) StartWatchers() {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	for _, cx := range s.galleryState().contexts {
		cx.StartBackground(s.cfg.Gallery.WatchEnabled, s.cfg.Gallery.MaxFileSizeMB, s.ingestNaming(cx.Name), s.jobs)
		go cx.WarmCaches()
	}
}

// Handler returns the root HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	rt := s.newRoutes()
	s.registerRoutes(rt)

	// Middleware order, outermost first: logging, session, first-run gate,
	// CSRF. The gallery lock is not among them any more - every route takes
	// it at its own registration, or names the reason it does not.
	var h http.Handler = rt.mux
	h = s.cSRFMiddleware(h)
	h = s.setupMiddleware(h)
	h = s.sessionMiddleware(h)
	h = loggingMiddleware(h)

	return h
}

// registerRoutes wires every route with the gallery-context mode it needs.
func (s *Server) registerRoutes(rt *routes) {
	// The assets are free because none of them reads a gallery row; the
	// thumbnail route resolves a context only to take its directory.
	rt.free("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.staticFS))).ServeHTTP)
	rt.free("GET /theme.css", s.serveThemeCSS)
	rt.free("GET /theme.logo", s.serveThemeLogo)
	rt.free("GET /theme.favicon", s.serveThemeFavicon)
	rt.free("GET /manifest.json", s.manifestHandler)
	rt.free("GET /thumbnails/{gallery}/{file}", s.serveThumbnail)
	// Fallback icon for tabs with no <link rel="icon"> (a raw image opened
	// in a new tab). Route through the override so server.logo applies;
	// non-permanent since that target can change.
	rt.read("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.booruFaviconURL(), http.StatusFound)
	})

	rt.read("GET /health", func(w http.ResponseWriter, r *http.Request) {
		// A liveness probe is not worth refusing over its Origin: the browser
		// enforces the block on its own, and a monitor that happens to send
		// one should still get an answer.
		api.SetCORS(w, r, s.cfgSnapshot())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// "app" is what lets a second launch tell our own instance from
		// whatever else already took the port.
		_ = json.NewEncoder(w).Encode(map[string]string{"app": "monbooru", "status": "ok", "version": Version})
	})

	rt.read("GET /login", s.loginPage)
	rt.read("POST /login", s.loginPost)
	rt.read("POST /logout", s.logoutPost)

	rt.read("POST /upload", s.uploadPost)

	// Root only; `GET /` below is the catch-all for unmatched paths. The
	// `/{$}` pattern wins over `/` for the exact root.
	rt.read("GET /{$}", s.galleryHandler)
	rt.read("GET /", s.notFoundHandler)

	// Switches the active gallery when the image lives in another one.
	rt.write("GET /i/{sha}", s.imageByHashHandler)
	rt.read("GET /images/{id}", s.detailHandler)
	rt.read("GET /images/{id}/related", s.relatedImagesHandler)
	rt.read("GET /images/{id}/file", s.serveImageFile)
	rt.read("GET /images/{id}/view", s.serveImageView)
	rt.read("GET /images/{id}/page/{n}", s.serveMangaPage)
	rt.read("GET /images/{id}/page/{n}/thumb", s.serveMangaPageThumb)
	rt.read("POST /images/{id}/page/{n}/extract", s.extractMangaPage)
	rt.read("POST /images/{id}/generate-collection", s.generateMangaCollection)
	rt.read("GET /images/{id}/read", s.readerHandler)
	rt.read("GET /images/{id}/pages", s.pagesGridHandler)
	rt.read("POST /images/{id}/tags", s.addTagToImage)
	rt.read("DELETE /images/{id}/tags", s.removeAllTagsFromImageHandler)
	rt.read("DELETE /images/{id}/user-tags", s.removeUserTagsFromImageHandler)
	rt.read("DELETE /images/{id}/auto-tags", s.removeAutoTagsFromImageHandler)
	rt.read("DELETE /images/{id}/source-tags", s.removeSourceTagsFromImageHandler)
	rt.read("DELETE /images/{id}/stale-tags", s.removeStaleTagsFromImageHandler)
	rt.read("DELETE /images/{id}/category-tags", s.removeCategoryTagsFromImageHandler)
	rt.read("DELETE /images/{id}/source-contribution", s.dropSourceContributionHandler)
	rt.read("DELETE /images/{id}/tags/{tagID}", s.removeTagFromImage)
	rt.read("POST /images/{id}/favorite", s.toggleFavorite)
	rt.read("POST /images/{id}/inbox", s.toggleInbox)
	rt.read("DELETE /images/{id}", s.deleteImage)
	rt.read("POST /images/{id}/canonical-path", s.promoteCanonical)
	rt.read("POST /images/{id}/sources/set", s.setSource)
	rt.read("POST /images/{id}/sources/remove", s.removeSource)
	rt.read("POST /images/{id}/sources/primary", s.makeSourcePrimary)
	rt.read("POST /images/{id}/sources/keep", s.keepLocalFile)
	rt.read("POST /images/{id}/annotations/set", s.setAnnotation)
	rt.read("POST /images/{id}/annotations/remove", s.removeAnnotation)
	rt.read("POST /images/{id}/markup/preview", s.previewMarkup)
	// The peer-bound routes are free so the outbound call is not made under
	// a read lock a gallery switch would then queue behind; each reads its
	// own rows under a short lock instead.
	rt.free("POST /images/{id}/sources/fetch", s.fetchSource)
	rt.free("POST /images/{id}/lookup", s.lookupImage)
	rt.free("POST /images/{id}/replace", s.replaceImage)
	rt.free("GET /images/{id}/ptr-contrib-panel", s.ptrContribPanel)
	rt.free("GET /images/{id}/ptr-contrib-dialog", s.ptrContribDialog)
	rt.free("POST /images/{id}/ptr-contrib", s.ptrContribSend)
	rt.read("POST /images/{id}/note", s.setNote)
	rt.read("POST /internal/batch-lookup/count", s.batchLookupCount)
	rt.read("POST /images/{id}/scheduled-lookup", s.scheduledLookupPost)
	rt.read("POST /images/{id}/scheduled-lookup/reset", s.scheduledLookupResetPost)
	rt.read("POST /images/{id}/commentary/set", s.setSourceCommentary)
	rt.read("POST /images/{id}/commentary/remove", s.removeSourceCommentary)
	rt.read("POST /images/{id}/translation/set", s.setSourceTranslation)
	rt.read("POST /images/{id}/translation/remove", s.removeSourceTranslation)
	rt.read("POST /images/{id}/original/set", s.setSourceOriginal)
	rt.read("POST /images/{id}/original/remove", s.removeSourceOriginal)
	rt.read("POST /images/{id}/collections/set", s.setCollection)
	rt.read("POST /images/{id}/collections/remove", s.removeCollection)
	rt.read("POST /images/{id}/place", s.placeImage)
	rt.read("POST /images/{id}/transfer", s.transferImage)
	rt.read("DELETE /images/{id}/aliases/{pathID}", s.deleteAlias)

	rt.read("GET /tags", s.tagsHandler)
	rt.read("GET /tags/{id}", s.tagDetailHandler)
	rt.read("GET /tags/{id}/usage", s.tagUsagePanelHandler)
	rt.read("POST /tags/batch-category", s.batchTagCategoryPost)
	rt.read("POST /tags/batch-alias", s.batchTagAliasPost)
	rt.read("POST /tags/merge-folded", s.batchMergeFoldedPost)
	rt.read("POST /tags/batch-imply", s.batchTagImplyPost)
	rt.read("POST /tags/new", s.createTagPost)
	rt.read("POST /tags/aliases", s.createAliasPost)
	rt.read("POST /tags/{id}/rename", s.renameTagPost)
	rt.read("DELETE /tags/{id}", s.deleteTagHandler)
	rt.read("PATCH /tags/{id}/category", s.changeTagCategory)
	rt.read("GET /tags/{id}/implications", s.implicationsDialogHandler)
	rt.read("POST /tags/{id}/implications", s.addImplicationPost)
	rt.read("POST /tags/{id}/implied-by", s.addImpliedByPost)
	rt.read("POST /tags/{id}/aliases", s.addTagAliasPost)
	// The group deletes carry the /group suffix because a bare
	// `DELETE /tags/{id}/aliases` overlaps `DELETE /tags/categories/{id}`
	// with neither pattern more specific, which the mux refuses.
	rt.read("DELETE /tags/{id}/implications/group", s.removeImplicationsDelete)
	rt.read("DELETE /tags/{id}/implied-by/group", s.removeImpliedByDelete)
	rt.read("DELETE /tags/{id}/aliases/group", s.removeTagAliasesDelete)
	rt.read("DELETE /tags/{id}/implications/{impliedID}", s.removeImplicationDelete)
	rt.read("POST /tags/categories", s.createCategoryPost)
	rt.read("POST /tags/categories/{id}/rename", s.renameCategoryPost)
	rt.read("DELETE /tags/categories/{id}", s.deleteCategoryDelete)
	rt.read("GET /tags/categories/{id}/count", s.categoryCountHandler)

	rt.read("GET /collections", s.collectionsHandler)
	rt.read("POST /collections/rename", s.renameCollectionPost)
	rt.read("POST /collections/dissolve", s.dissolveCollectionPost)
	rt.read("POST /collections/find-relations", s.collectionFindRelationsPost)
	rt.read("GET /collections/order", s.collectionOrderDialog)
	rt.read("POST /collections/order", s.reorderCollectionPost)
	rt.read("POST /collections/generate-cbz", s.generateCollectionCBZ)

	rt.read("GET /categories", s.categoriesHandler)

	rt.read("GET /settings", s.settingsHandler)
	rt.read("POST /settings/general", s.settingsGeneralPost)
	rt.read("POST /settings/monloader", s.settingsMonloaderPost)
	rt.read("POST /settings/tagger", s.settingsTaggerPost)
	rt.read("POST /settings/auth/password", s.settingsPasswordPost)
	rt.read("POST /settings/auth/remove-password", s.settingsRemovePasswordPost)
	rt.read("POST /settings/auth/tokens", s.settingsTokenCreate)
	rt.read("DELETE /settings/auth/tokens/{id}", s.settingsTokenRevoke)
	rt.read("GET /settings/auth/tokens/{id}/privileges", s.settingsTokenPrivilegesGet)
	rt.read("POST /settings/auth/tokens/{id}/privileges", s.settingsTokenPrivilegesPost)
	rt.read("PATCH /settings/categories/{id}", s.updateCategoryPatch)
	rt.read("POST /settings/schedule", s.settingsSchedulePost)
	rt.read("POST /settings/schedule/run", s.settingsScheduleRunPost)
	rt.read("GET /setup", s.setupPage)
	// The wizard's submit repoints the default gallery.
	rt.write("POST /setup", s.setupPost)
	rt.read("GET /internal/browse", s.browseDirs)
	rt.read("POST /internal/open-folder", s.openFolder)
	rt.read("POST /settings/desktop", s.settingsDesktopPost)
	rt.read("POST /settings/quit", s.settingsQuit)
	rt.read("POST /settings/restart", s.settingsRestart)
	rt.read("POST /settings/maintenance/prune-missing", s.pruneMissingImagesPost)
	rt.read("POST /settings/maintenance/prune-orphaned-thumbnails", s.pruneOrphanedThumbnailsPost)
	rt.read("POST /settings/maintenance/empty-folders", s.emptyFoldersScanPost)
	rt.read("POST /settings/maintenance/empty-folders/remove", s.emptyFoldersRemovePost)
	rt.read("POST /settings/maintenance/recalc-tags", s.recalcTagsPost)
	rt.read("POST /settings/maintenance/tag-conflicts", s.tagCategoryConflictsPost)
	rt.read("POST /settings/maintenance/find-folded-duplicates", s.findFoldedDuplicatesPost)
	rt.read("POST /settings/maintenance/lookup-due", s.lookupDuePost)
	// Relocated to /relations/file-duplicates/* in v1.8; old routes
	// stay alive as 301 redirects for one release so bookmarks survive.
	rt.read("GET /settings/maintenance/duplicates-list", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/relations/file-duplicates/list", http.StatusMovedPermanently)
	})
	rt.read("POST /settings/maintenance/remove-duplicates", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/relations/file-duplicates/remove", http.StatusMovedPermanently)
	})
	rt.read("GET /relations/file-duplicates/list", s.duplicatesListHandler)
	rt.read("POST /relations/file-duplicates/remove", s.removeDuplicatesPost)
	rt.read("POST /relations/file-duplicates/promote", s.promoteAliasPathPost)
	rt.read("GET /relations/duplicates/sha256", s.sha256WalkerPage)
	rt.read("POST /relations/duplicates/sha256/remove-one", s.sha256WalkerRemoveOnePost)
	rt.read("GET /relations/duplicates/marked", s.markedWalkerPage)
	rt.read("POST /relations/duplicates/marked/delete-one", s.markedWalkerDeleteOnePost)
	rt.read("POST /relations/duplicates/marked/delete-all", s.markedWalkerDeleteAllPost)
	rt.read("GET /relations", s.relationsPage)
	rt.read("GET /relations/browse", s.browseRelationsPage)
	rt.read("GET /relations/browse-groups", s.browseGroupsRedirect)
	rt.read("GET /relations/session", s.sessionPage)
	rt.read("POST /relations/session/decide", s.sessionDecidePost)
	rt.read("POST /relations/dup-group/{id}/copy-tags", s.copyTagsToOriginalPost)
	rt.read("GET /relations/dup-group/{id}/copy-tags/preview", s.copyTagsToOriginalPreview)
	rt.read("POST /settings/relations", s.settingsRelationsPost)
	rt.read("POST /settings/maintenance/re-extract-metadata", s.reExtractMetadataPost)
	rt.read("POST /settings/maintenance/rebuild-thumbnails", s.rebuildThumbnailsPost)
	rt.read("POST /settings/maintenance/compute-hashes", s.computeHashesPost)
	rt.read("POST /relations/find-pairs", s.findRelationPairsPost)
	rt.read("POST /relations/reset-skipped", s.resetSkippedPost)
	rt.read("POST /relations/phash/{id}/recompute", s.recomputePhashPost)
	rt.read("POST /relations/add", s.addRelationPost)
	rt.read("POST /relations/remove", s.removeRelationPost)
	rt.read("POST /relations/reverse", s.reverseRelationPost)
	rt.read("POST /relations/browse-groups/merge", s.mergeGroupsPost)
	rt.read("POST /relations/browse-groups/dissolve", s.dissolveGroupsPost)
	rt.read("GET /internal/images/{id}/md5", s.md5CellGet)
	rt.read("GET /internal/images/{id}/related-entries", s.relatedEntriesGet)
	rt.read("GET /internal/images/{id}/fetch-status", s.fetchStatusHandler)
	rt.read("GET /images/{id}/relations", s.imageRelationsPage)
	rt.read("POST /settings/maintenance/vacuum-db", s.vacuumDBPost)
	rt.read("POST /settings/maintenance/free-memory", s.freeMemoryPost)
	rt.read("POST /settings/tagger/{name}/enable", s.settingsTaggerEnablePost)
	rt.read("POST /settings/tagger/{name}/disable", s.settingsTaggerDisablePost)
	rt.read("POST /settings/tagger/{name}/delete", s.settingsTaggerDeletePost)
	rt.read("GET /settings/tagger/{name}/config", s.settingsTaggerConfigGet)
	rt.read("POST /settings/tagger/{name}/config", s.settingsTaggerConfigPost)
	rt.read("GET /settings/tagger/{name}/labels", s.settingsTaggerLabelsGet)
	rt.read("POST /settings/tagger/{name}/mapping", s.settingsTaggerMappingPost)
	rt.read("POST /settings/tagger/{name}/reset", s.settingsTaggerResetPost)

	// Saved searches are managed from the sidebar (no dedicated search page).
	rt.read("POST /search/saved", s.createSavedSearch)
	rt.read("DELETE /search/saved/{id}", s.deleteSavedSearch)

	rt.read("GET /internal/job/status", s.jobStatusHandler)
	rt.free("GET /internal/monloader-status", s.monloaderStatusHandler)
	rt.read("POST /internal/job/dismiss", s.jobDismissPost)
	rt.read("POST /internal/job/cancel", s.jobCancelPost)
	rt.read("POST /internal/sync", s.syncTrigger)
	rt.read("POST /internal/autotag", s.autotagTrigger)
	rt.read("POST /internal/batch-delete", s.batchDelete)
	rt.read("POST /internal/batch-place", s.batchPlace)
	rt.read("POST /internal/batch-transfer", s.batchTransfer)
	rt.read("POST /internal/batch-tag", s.batchTag)
	rt.read("POST /internal/batch-strip", s.batchStrip)
	rt.read("POST /internal/batch-inbox", s.batchInbox)
	rt.read("POST /internal/batch-favorite", s.batchFavorite)
	rt.read("POST /internal/batch-collection", s.batchCollection)
	rt.read("POST /internal/batch-lookup", s.batchLookup)
	rt.read("POST /internal/delete-search", s.deleteSearchPost)
	rt.read("POST /tags/delete-search", s.deleteTagsSearchPost)
	rt.free("POST /tags/ptr-lookup-search", s.ptrLookupSearchPost)
	rt.free("GET /tags/{id}/ptr-contrib-panel", s.tagPtrContribPanel)
	rt.free("GET /tags/{id}/ptr-contrib-dialog", s.tagPtrContribDialog)
	rt.free("POST /tags/{id}/ptr-contrib", s.tagPtrContribSend)
	rt.read("GET /tags/{id}/ptr-lookup-dialog", s.tagPtrLookupDialog)
	rt.free("GET /tags/{id}/ptr-lookup-preview", s.tagPtrLookupPreview)
	rt.free("GET /tags/{id}/ptr-lookup-search", s.tagPtrLookupSearch)
	rt.read("POST /tags/{id}/ptr-lookup", s.ptrLookupTagPost)
	rt.read("GET /internal/tags/suggest", s.tagSuggest)
	rt.read("GET /internal/search/suggest", s.searchSuggest)
	rt.read("GET /internal/search/ids", s.searchIDs)
	rt.read("GET /internal/folders/suggest", s.foldersSuggest)
	rt.read("GET /internal/collection/suggest", s.collectionSuggest)
	rt.read("GET /internal/source/suggest", s.sourceSuggest)
	rt.read("GET /internal/name/preview", s.namePreview)
	rt.read("GET /internal/sidebar", s.gallerySidebar)
	rt.read("GET /internal/sidebar-browse", s.sidebarBrowse)
	rt.read("POST /internal/rating-ceiling", s.ratingCeilingPost)
	rt.read("POST /internal/view-prefs", s.viewPrefsPost)
	rt.read("POST /images/{id}/autotag", s.autotagImage)
	rt.read("GET /images/{id}/tags", s.getImageTagsHandler)

	// The gallery mutations take the write lock themselves. The export is
	// not one of them: it streams a whole database out and holds the read
	// lock for the length of it, so a removal waits rather than closing the
	// handles mid-stream.
	rt.write("POST /internal/gallery/switch", s.gallerySwitchHandler)
	rt.write("POST /settings/galleries", s.settingsGalleriesPost)
	rt.write("POST /settings/galleries/{name}/rename", s.settingsGalleryRenamePost)
	rt.write("POST /settings/galleries/{name}/delete", s.settingsGalleryDeletePost)
	rt.write("POST /settings/galleries/{name}/default", s.settingsGalleryDefaultPost)
	rt.read("GET /settings/galleries/{name}/export", s.settingsGalleryExport)
	rt.write("POST /settings/galleries/{name}/import", s.settingsGalleryImport)

	// The /api/v1 namespace's other three routes. They are here and not in
	// internal/api because pairing writes the config and the plugin
	// registry, which the REST handler does not hold - moving the routes
	// would move that state. They answer through api.WriteJSON and
	// api.SetCORS so a peer sees one namespace either way.
	rt.read("POST /api/v1/pair/request", s.pairRequest)
	rt.read("GET /api/v1/pair/status", s.pairStatus)
	rt.read("POST /api/v1/pair/remove", s.pairTeardown)
	rt.read("GET /internal/plugins/pairing", s.pluginPairingFragment)
	// Free for the same outbound reason: the plugin mount is the longest of
	// them, serving a peer's page for as long as the peer takes.
	rt.free("POST /settings/plugins/pair/{id}/approve", s.pluginPairApprove)
	rt.free("POST /settings/plugins/pair/{id}/deny", s.pluginPairDeny)
	rt.free("POST /settings/plugins/{name}/remove", s.pluginPairRemove)
	rt.free("POST /settings/plugins/{name}/pause", s.pluginPause)
	rt.free("POST /settings/plugins/{name}/start", s.pluginStart)
	rt.free("POST /settings/plugins/{name}/stop", s.pluginStop)
	rt.free("POST /settings/plugins/theme", s.settingsThemePost)
	rt.free("POST /internal/monloader/disconnect", s.monloaderLightDisconnect)
	rt.free("POST /internal/monloader/reconnect", s.monloaderLightReconnect)
	rt.free("POST /internal/plugin/relay", s.pluginRelay)
	// What a page needs: its own GETs (HEAD rides along) and its form posts.
	rt.free("GET "+pluginMountPrefix+"{name}/", s.pluginMount)
	rt.free("POST "+pluginMountPrefix+"{name}/", s.pluginMount)

	// The API package registers on a mux of its own so its routes ride the
	// read mode as a subtree instead of reaching past the modes. The methods
	// are spelled out because a bare "/api/v1/" answers more of them than
	// the catch-all "GET /" does, which the mux refuses as a conflict.
	apiMux := http.NewServeMux()
	api.New(s.cfgSnapshot, s.jobs, s.apiResolver, Version).Mount(apiMux)
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"} {
		rt.read(method+" /api/v1/", apiMux.ServeHTTP)
	}
}

// allContexts lists every open gallery.
func (s *Server) allContexts() []*galleryCtx {
	st := s.galleryState()
	out := make([]*galleryCtx, 0, len(st.contexts))
	for _, cx := range st.contexts {
		out = append(out, cx)
	}
	return out
}

// apiResolver looks up a gallery by name for the API package. Empty name
// falls back to the active gallery.
func (s *Server) apiResolver(name string) (api.Gallery, bool) {
	var cx *galleryCtx
	if name == "" {
		cx = s.active()
	} else {
		cx = s.get(name)
	}
	if cx == nil {
		return api.Gallery{}, false
	}
	return api.Gallery{
		Handle:           cx.Handle,
		RelationsSvc:     cx.RelationsSvc,
		InvalidateCaches: cx.InvalidateCaches,
		RecordFetch:      func(id int64, state, msg string) { s.fetchStatus.record(cx.Name, id, state, msg) },
	}, true
}

// isNoisyPath reports paths that are requested constantly (polling, static
// assets, thumbnails, health probes). They log at debug so the default info
// level stays readable.
func isNoisyPath(path string) bool {
	switch path {
	case "/internal/job/status", "/internal/monloader-status", "/health":
		return true
	}
	return strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/thumbnails/")
}

// requestStartKey carries the wall-clock time at which the outermost
// middleware first saw the request. base() reads it back so the footer's
// "page loaded in N ms" reflects everything between the request hitting
// our handler chain and the layout footer rendering, not just the
// handler's tail end.
type requestStartKey struct{}

func requestStartFromContext(ctx context.Context) time.Time {
	if t, ok := ctx.Value(requestStartKey{}).(time.Time); ok {
		return t
	}
	return time.Time{}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), requestStartKey{}, time.Now()))
		if isNoisyPath(r.URL.Path) {
			logx.Debugf("%s %s", r.Method, r.URL.Path)
		} else {
			logx.Infof("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// Version is set at build time via -ldflags, or read from VERSION.md.
var Version = "dev"

// RepoURL is the canonical git repository URL, set at build time via -ldflags.
var RepoURL = "https://github.com/monbooru/monbooru"

// DocURL is the online documentation URL, set at build time via -ldflags from
// DOC.md.
var DocURL = "https://monbooru.github.io/mondocs/index.html"

// Variant identifies the build flavour (e.g. "cuda") and is injected at
// build time via -ldflags from the CUDA Dockerfile. Empty for the default
// CPU build; rendered in parentheses in the footer when non-empty.
var Variant = ""

// Package names whatever produced this artifact ("docker", "tarball", "zip",
// "installer", "flatpak", "appimage"), injected at build time via -ldflags.
// With several artifacts in circulation every bug report opens on the
// question it answers.
// "source" is a plain go build and renders as nothing.
var Package = "source"

// BuildLabel joins the two build stamps for the footer and -version, so
// which artifact and which provider are one parenthesis rather than two.
func BuildLabel() string {
	parts := make([]string, 0, 2)
	if Package != "" && Package != "source" {
		parts = append(parts, Package)
	}
	if Variant != "" {
		parts = append(parts, Variant)
	}
	return strings.Join(parts, ", ")
}

// ratingLevel is one cell in the footer rating selector. Value is the
// underlying tag name (used as the cookie value and the AST key); Label
// is the user-facing text - "general" renders as "sfw" so the toggle
// doesn't lead with the implicit-default name.
type ratingLevel struct {
	Value string
	Label string
}

// ratingFooterLevels is the fixed left-to-right footer order, low to
// high. Active is "explicit" when the cookie is unset (no ceiling).
var ratingFooterLevels = []ratingLevel{
	{Value: "general", Label: "sfw"},
	{Value: "sensitive", Label: "sensitive"},
	{Value: "questionable", Label: "questionable"},
	{Value: "explicit", Label: "explicit"},
}

// baseData is common template data present on every page.
type baseData struct {
	Title       string
	ActiveNav   string
	CSRFToken   string
	AuthEnabled bool
	Degraded    bool
	Version     string
	RepoURL     string
	DocURL      string
	// Build is the artifact-and-provider stamp rendered beside the version;
	// empty on a plain source build.
	Build string
	// Theme is true while an operator-installed theme resolves, gating the
	// /theme.css link that follows the bundled sheet.
	Theme bool
	// SidebarCollapsed hides the sidebar column on the layout's first paint.
	// Rendered server-side so a navigation doesn't flash the column in and
	// back out once main.js runs.
	SidebarCollapsed bool
	// BooruName is the operator's brand override (or "Monbooru" by
	// default). Rendered into every page <title>, the topbar wordmark,
	// and the login screen so a deployment that wants a different name
	// only edits monbooru.toml.
	BooruName string
	// BooruLogo is the resolved URL for the topbar logo: the active
	// theme's "/theme.logo" when it ships one, the bundled logo.png
	// otherwise. BooruFavicon is the same for the favicon <link>, taking
	// the theme's "/theme.favicon" when it ships one. A theme moves each
	// surface only through the file drawn for it.
	BooruLogo    string
	BooruFavicon string
	// MonloaderURL is the browser-facing monloader base for the footer
	// "connected to monloader" link, trailing slash trimmed; falls back to
	// the api url when unset, so only both being unset drops the link.
	MonloaderURL  string
	ActiveGallery string
	Galleries     []config.Gallery
	// Counts surfaced on the footer status bar. Populated per-request;
	// zero when the active gallery is missing or a query failed.
	VisibleCount     int
	InboxCount       int
	TagCount         int
	CollectionsCount int
	// InboxNavActive marks the top-nav "Inbox" entry as the active
	// page when the current URL's `q` parameter positively asserts
	// inbox:true at the top level. Same parser-based gate the inline
	// upload drop zone uses.
	InboxNavActive bool
	// HiddenByCeiling drives the "N hidden images in the current search"
	// footer cell. Only the gallery handler populates it; on every other
	// page the field stays at 0 and the cell renders empty.
	HiddenByCeiling int
	// Rating ceiling state for the footer selector. ActiveRating is the
	// effective level - "explicit" when no cookie is set.
	RatingLevels []ratingLevel
	ActiveRating string
	// RequestStart is the wall-clock time captured by loggingMiddleware
	// when the request first entered our handler chain. The footer
	// renders time.Since(RequestStart) so the indicator covers all
	// middleware + handler work + template execution, not just the
	// tail-end after base() runs.
	RequestStart time.Time
	// MonloaderPaired gates the footer "connected to monloader" light:
	// it renders (and starts polling) only while a monloader pairing exists.
	MonloaderPaired bool
	// MonloaderUsable gates the monloader-backed actions (online lookup, find
	// tags, source refetch): paired and the link is neither paused nor a probe
	// found it unreachable / rejecting.
	MonloaderUsable bool
	// MonloaderConn / MonloaderVersion seed the light on the initial render;
	// the poller swaps in live values. They live here so the partial resolves
	// on every page struct, not just the poll handler's map.
	MonloaderConn    string
	MonloaderVersion string
	// MonloaderPTR gates the PTR-backed lookup controls: true when the last
	// cached probe saw monloader report its PTR index enabled and caught up,
	// the only state it answers a read in. Stale reads are fine - monloader
	// answers 409 and the UI degrades in place.
	MonloaderPTR bool
	// MonloaderContrib gates every PTR contribution surface: true when the
	// last cached probe saw monloader report a usable personal account
	// (contrib.account && !contrib.banned). An absent contrib field on an
	// older monloader reads as false, so contribution UI never renders
	// against a monloader that can't serve it. Stale reads degrade in
	// place on the 409, like the lookup gating.
	MonloaderContrib bool
	// MonloaderPTRSyncing caveats the lookup backend dialog while the PTR
	// index is still building: it answers on partial data by design.
	MonloaderPTRSyncing bool
	// MonloaderPTRPresent gates the PTR-backed surfaces' render, where
	// MonloaderPTR gates whether they are live. A paused or unreachable
	// link reports no PTR either way, so the flag has to say "and we
	// cannot currently tell" or the surfaces vanish on a pause.
	MonloaderPTRPresent bool
}

// sidebarCookieName records the collapsed sidebar. The topbar toggle in
// main.js writes it; only the layout reads it.
const sidebarCookieName = "monbooru_sidebar"

// sidebarCollapsed reports whether the operator hid the sidebar. Anything
// other than the one stored value reads as shown, so a stale or
// hand-edited cookie can't leave the column missing with no way back.
func sidebarCollapsed(r *http.Request) bool {
	c, err := r.Cookie(sidebarCookieName)
	return err == nil && c.Value == "collapsed"
}

func (s *Server) base(r *http.Request, nav, title string) baseData {
	sessID := sessionFromContext(r.Context())
	cx := s.active()
	degraded := false
	visible, inbox, tagCount, collectionsCount := 0, 0, 0, 0
	if cx != nil {
		degraded = cx.Degraded
		visible, _ = cx.VisibleCount()
		// Inbox count is ceiling-aware here because every surface that
		// renders it (top-nav "Inbox (N)" link, inline drop zone, search
		// suggestions) promises the post-click match count.
		inbox, _ = cx.InboxCountUnder(resolveCeiling(r, cx))
		tagCount, _ = cx.TagCount()
		collectionsCount, _ = cx.CollectionsCount()
	}
	inboxNavActive := false
	if expr, parseErr := search.Parse(r.URL.Query().Get("q")); parseErr == nil {
		inboxNavActive = inboxFilterActive(expr)
	}
	galleries := s.galleries()
	active := readRatingCookie(r)
	active = cmp.Or(active, "explicit")
	ml := s.mlStatus.Seed()
	if s.monloaderPaused() {
		// A paused link renders as paused everywhere and hides the
		// PTR-gated surfaces, regardless of the last probe's cache.
		ml = monloader.Status{Conn: "paused"}
	}
	paired := s.pairedWith("monloader")
	monloaderUsable := s.monloaderUsable()
	themeSheet := s.activeTheme().Path != ""
	return baseData{
		Title:               title,
		ActiveNav:           nav,
		CSRFToken:           s.csrfToken(sessID),
		AuthEnabled:         s.authEnabled(),
		Degraded:            degraded,
		Version:             Version,
		RepoURL:             RepoURL,
		DocURL:              DocURL,
		Build:               BuildLabel(),
		Theme:               themeSheet,
		SidebarCollapsed:    sidebarCollapsed(r),
		BooruName:           s.booruName(),
		BooruLogo:           s.booruLogoURL(),
		BooruFavicon:        s.booruFaviconURL(),
		MonloaderURL:        s.monloaderWebBase(),
		MonloaderPaired:     paired,
		MonloaderUsable:     monloaderUsable,
		MonloaderConn:       ml.Conn,
		MonloaderVersion:    ml.Version,
		MonloaderPTR:        ml.PTR,
		MonloaderPTRSyncing: ml.PTRSyncing,
		MonloaderPTRPresent: paired && (ml.PTR || ml.PTRSyncing || !monloaderUsable),
		MonloaderContrib:    ml.Contrib,
		ActiveGallery:       s.activeGallery(),
		Galleries:           galleries,
		VisibleCount:        visible,
		InboxCount:          inbox,
		TagCount:            tagCount,
		CollectionsCount:    collectionsCount,
		InboxNavActive:      inboxNavActive,
		RatingLevels:        ratingFooterLevels,
		ActiveRating:        active,
		RequestStart:        requestStartFromContext(r.Context()),
	}
}

func (s *Server) renderTemplate(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Buffer so we can still send a clean 500 when template execution fails;
	// streaming directly into w would leak partial output and race with
	// http.Error (producing "superfluous response.WriteHeader" warnings).
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		logx.Errorf("template %q: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	if _, err := buf.WriteTo(w); err != nil {
		// Client disconnected mid-write. Nothing to do but log.
		logx.Warnf("template %q write: %v", name, err)
	}
}

// thumbnailNameRe matches the two on-disk filename patterns emitted by the
// thumbnail pipeline: `{id}.jpg` for static previews and `{id}_hover.webp`
// for animated hovers. Anything else under the thumbnails directory (stray
// files, editor backups, etc.) is not served.
var thumbnailNameRe = regexp.MustCompile(`^\d+(?:_hover\.webp|\.jpg)$`)

// serveThumbnail serves a thumbnail file from the named gallery's
// thumbnails directory. The gallery name is part of the URL so each
// gallery's thumbnails live at distinct URLs and the browser cache can't
// show a stale preview from another gallery after a switch.
func (s *Server) serveThumbnail(w http.ResponseWriter, r *http.Request) {
	file := filepath.Base(r.PathValue("file"))
	if !thumbnailNameRe.MatchString(file) {
		http.NotFound(w, r)
		return
	}
	cx := s.get(r.PathValue("gallery"))
	if cx == nil {
		http.NotFound(w, r)
		return
	}
	fullPath := filepath.Join(cx.ThumbnailsPath, file)
	// Hover variants are generated by ffmpeg after the static thumb and are
	// absent for recently-ingested animated files; static thumbs are absent
	// when generation failed on an undecodable file. Respond 204 so the img
	// tag's onerror still fires but the console doesn't log a 404 per card.
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// SQLite reuses the highest deleted INTEGER PRIMARY KEY id, so a
	// deleted image's URL can be reborn the next ingest with brand-new
	// thumbnail bytes at the same path. Without revalidation the browser
	// keeps serving the prior bytes from cache (heuristic freshness on a
	// bare Last-Modified). The ETag includes the file mtime so a rewrite
	// invalidates the cached response; same trick serveImageFile uses.
	setGalleryScopedCache(w, r.PathValue("gallery"), file, fullPath)
	http.ServeFile(w, r, fullPath)
}

// serveConfiguredFile serves one file of an installed theme off disk. An
// empty path 404s so the layout's gated <link> and the bundled-asset
// fallbacks degrade cleanly when the theme ships no such file. The cache
// tag revalidates against the file mtime so an edited file is picked up at
// once; a bare Last-Modified would go heuristically stale until the
// operator disabled the browser cache.
func (s *Server) serveConfiguredFile(w http.ResponseWriter, r *http.Request, path, kind string) {
	if path == "" {
		http.NotFound(w, r)
		return
	}
	setGalleryScopedCache(w, "custom", kind, path)
	http.ServeFile(w, r, path)
}

// booruName resolves server.name with a "Monbooru" fallback so every
// title-suffix callsite reads a single source of truth instead of
// repeating the default.
func (s *Server) booruName() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if name := s.cfg.Server.BooruName; name != "" {
		return name
	}
	return "Monbooru"
}

// modelPath reads paths.model_path under the config lock. Fixed after
// boot, but every tagger handler reaches for it and one lock discipline
// beats nine open-coded reads.
func (s *Server) modelPath() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Paths.ModelPath
}

// The settings handlers rewrite these fields at runtime under the write
// lock, so every serving path reads them under the read lock - through one
// of the accessors below, or through a caller that takes several at once
// (ingestNaming, receivedNaming). A string field is two words, and a torn
// read of one yields a slice header that never existed.

func (s *Server) authEnabled() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Auth.EnablePassword
}

func (s *Server) passwordHash() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Auth.PasswordHash
}

func (s *Server) sessionLifetimeDays() int {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Auth.SessionLifetimeDays
}

// thumbSizeCookieName records the grid's cell-size step.
const thumbSizeCookieName = "monbooru_thumb_size"

// thumbSize reads the step for this request. "m" is the default and the
// only one that renders no class; anything outside the closed set reads as
// "m", so a stale cookie cannot leave the grid at a size nothing offers.
func thumbSize(r *http.Request) string {
	c, err := r.Cookie(thumbSizeCookieName)
	if err != nil {
		return "m"
	}
	switch c.Value {
	case "s", "l":
		return c.Value
	}
	return "m"
}

// pageSizeCookieName records a per-view page size. The gallery's Show
// select writes it through viewPrefsPost; pageSize reads it back.
const pageSizeCookieName = "monbooru_page_size"

// PageSizeOptions is the closed set the Show select offers and the only
// set the cookie is honoured for. ui.page_size stays the default; this is
// the operator overriding it for the session's browsing, which is why it
// is a cookie and not config.
var PageSizeOptions = []int{20, 40, 60, 100, 250, 500}

// pageSizeOverride is the per-browser size in force, or 0 when no cookie is
// set. A value outside the offered set is dropped rather than clamped, so a
// hand-edited cookie cannot ask for a page the budgets never covered.
func pageSizeOverride(r *http.Request) int {
	c, err := r.Cookie(pageSizeCookieName)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(c.Value)
	if err != nil || !slices.Contains(PageSizeOptions, n) {
		return 0
	}
	return n
}

// pageSize is the size of one listing page for this request: the browser's
// own override, else the configured default.
func (s *Server) pageSize(r *http.Request) int {
	if n := pageSizeOverride(r); n > 0 {
		return n
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.UI.PageSize
}

func (s *Server) thumbnailFit() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.UI.ThumbnailFit
}

func (s *Server) maxFileSizeMB() int {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Gallery.MaxFileSizeMB
}

// watcherSettings pairs the two knobs every startWatcher call passes, so
// a save landing between them cannot start a watcher on half of one.
func (s *Server) watcherSettings() (bool, int) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Gallery.WatchEnabled, s.cfg.Gallery.MaxFileSizeMB
}

func (s *Server) themeColor() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Server.ThemeColor
}

func (s *Server) defaultGallery() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.DefaultGallery
}

func (s *Server) derivePaths(name string) (string, string) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.DerivePaths(name)
}

func (s *Server) executionProvider() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Tagger.ExecutionProvider
}

// cfgSnapshot copies the config for readers that hold it past the lock -
// the tagger, which reads it for the length of a job, and the API layer,
// which has no lock of its own. The four slices the settings writers edit
// in place are cloned; everything they hold is replaced wholesale rather
// than written through, so one level is enough.
func (s *Server) cfgSnapshot() *config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	c := *s.cfg
	c.Galleries = slices.Clone(c.Galleries)
	c.Plugins = slices.Clone(c.Plugins)
	c.Auth.Tokens = slices.Clone(c.Auth.Tokens)
	c.Tagger.Taggers = slices.Clone(c.Tagger.Taggers)
	return &c
}

// galleries copies cfg.Galleries under the lock its mutators take. A
// slice-header read torn against a reallocating append pairs the old
// array pointer with the new length, so the copy walks off the end of
// the old backing array and hands garbage strings to a template.
func (s *Server) galleries() []config.Gallery {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	out := make([]config.Gallery, len(s.cfg.Galleries))
	copy(out, s.cfg.Galleries)
	return out
}

// booruLogoURL points the topbar logo at the active theme's logo.png,
// else the bundled asset.
func (s *Server) booruLogoURL() string {
	if s.activeTheme().Logo != "" {
		return "/theme.logo"
	}
	return "/static/logo.png"
}

// booruFaviconURL is the same for the favicon. A theme's logo.png does not
// reach it - it is drawn for the topbar and at 16px in a tab reduces to
// mush - so a theme that wants the tab too ships the icon it wants drawn
// there.
//
// The URL carries the file's version because a browser keeps favicons in a
// store of its own rather than the page cache, and Firefox loads them with
// revalidation off: on one fixed URL the first icon a profile saw is the
// icon it keeps, so switching themes changed everything but the tab. A
// version in the URL makes each icon a URL of its own. Nothing else linked
// off a theme needs it - a stylesheet and an <img> revalidate normally.
func (s *Server) booruFaviconURL() string {
	e := s.activeTheme()
	if e.Favicon == "" {
		return "/static/favicon.png"
	}
	return "/theme.favicon?v=" + themeFaviconVersion(e)
}

// themeFaviconVersion identifies the active theme's tab icon: the file's
// mtime for a copy on disk, which also moves when the operator edits it, and
// the build for a built-in, whose files only change with the binary.
func themeFaviconVersion(e themeEntry) string {
	if e.Builtin {
		return cmp.Or(Version, "builtin")
	}
	info, err := os.Stat(e.Favicon)
	if err != nil {
		return e.Name
	}
	return strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

// monloaderWebBase is the browser-facing monloader base for the footer
// "connected to monloader" link: the configured web url when set, else the api url.
func (s *Server) monloaderWebBase() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	base := s.cfg.Server.MonloaderURL
	base = cmp.Or(base, s.cfg.Monloader.APIURL)
	return strings.TrimRight(base, "/")
}

// uppercasePercentEscapes rewrites every %XX hex pair in s to use
// uppercase hex while leaving all other characters untouched. Used to
// align url.QueryEscape's lowercase output with the browser address
// bar's RFC 3986 normalization so the same logical query doesn't show
// up twice in autocomplete history.
func uppercasePercentEscapes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]) {
			b.WriteByte('%')
			b.WriteByte(toUpperHex(s[i+1]))
			b.WriteByte(toUpperHex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func toUpperHex(c byte) byte {
	if c >= 'a' && c <= 'f' {
		return c - 'a' + 'A'
	}
	return c
}

// resolveMangaImage looks up a manga row's canonical_path. Returns
// (path, true) when the row is a cbz; (_, false) for non-manga ids and
// missing rows. Callers respond 404 on the false return.
func (s *Server) resolveMangaImage(idStr string) (string, bool) {
	cx := s.active()
	if cx == nil {
		return "", false
	}
	var canonPath, fileType string
	if err := cx.DB.Read.QueryRow(
		`SELECT canonical_path, file_type FROM images WHERE id = ?`, idStr,
	).Scan(&canonPath, &fileType); err != nil {
		return "", false
	}
	if fileType != "cbz" {
		return "", false
	}
	// Refuse a canonical_path that drifted outside the gallery root before
	// the archive extractor opens it, mirroring serveImageFile.
	if !gallery.NamedInside(cx.GalleryPath, canonPath) {
		return "", false
	}
	return canonPath, true
}

// serveMangaPage serves the n-th page of a manga (1-based) from the
// per-image cache, extracting on miss. Cache-Control fixes browser
// behavior under prefetch / back-button so the same page isn't refetched
// constantly during reader navigation.
func (s *Server) serveMangaPage(w http.ResponseWriter, r *http.Request) {
	s.serveMangaPagePath(w, r, gallery.EnsureMangaPage, "")
}

// serveMangaPageThumb serves the n-th page's thumbnail (300px-longest-
// side JPEG) used by the pages-grid view. Same lazy-extract +
// idle-evict path as serveMangaPage.
func (s *Server) serveMangaPageThumb(w http.ResponseWriter, r *http.Request) {
	s.serveMangaPagePath(w, r, gallery.EnsureMangaPageThumb, "-thumb")
}

// serveMangaPagePath is the shared body behind serveMangaPage and
// serveMangaPageThumb. cacheSuffix keeps the thumb-vs-bytes cache keys
// disjoint so a gallery switch invalidates each independently.
func (s *Server) serveMangaPagePath(
	w http.ResponseWriter, r *http.Request,
	ensure func(thumbnailsPath, canonPath string, imageID int64, n int) (string, error),
	cacheSuffix string,
) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		http.NotFound(w, r)
		return
	}
	canonPath, ok := s.resolveMangaImage(idStr)
	if !ok {
		http.NotFound(w, r)
		return
	}
	page, err := ensure(s.thumbnailsPath(), canonPath, id, n)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Scope the cached bytes to the active gallery so a gallery switch
	// invalidates them; see serveImageFile for the same trick.
	setGalleryScopedCache(w, s.activeGallery(), fmt.Sprintf("%s-%d%s", idStr, n, cacheSuffix), page)
	http.ServeFile(w, r, page)
}

// serveImageFile serves the raw image/video file.
func (s *Server) serveImageFile(w http.ResponseWriter, r *http.Request) {
	s.serveImageBytes(w, r, false)
}

// serveImageView is what every viewer in the UI points at: the original
// bytes, or - for a still past the display ceiling, which no browser will
// decode - the bounded rendition instead. One endpoint rather than a
// template branch so the lightbox and the relations surfaces, which build
// their src in JS and hold no dimensions, get the same answer. The download
// link and the <video> src stay on /file: those are the bytes themselves.
func (s *Server) serveImageView(w http.ResponseWriter, r *http.Request) {
	s.serveImageBytes(w, r, true)
}

func (s *Server) serveImageBytes(w http.ResponseWriter, r *http.Request, scaled bool) {
	idStr := r.PathValue("id")
	cx := s.active()
	if cx == nil {
		http.NotFound(w, r)
		return
	}
	var id int64
	var canonPath, fileType string
	var width, height sql.NullInt64
	if err := cx.DB.Read.QueryRow(
		`SELECT id, canonical_path, file_type, width, height FROM images WHERE id = ?`, idStr,
	).Scan(&id, &canonPath, &fileType, &width, &height); err != nil {
		http.NotFound(w, r)
		return
	}
	// A canonical_path that drifted outside the gallery root does not get
	// opened, whatever put it there.
	if !gallery.NamedInside(cx.GalleryPath, canonPath) {
		http.NotFound(w, r)
		return
	}
	// /images/{id}/file is the same URL across galleries, so the
	// browser's cache key alone can't tell them apart - switching
	// galleries used to keep showing the prior gallery's id=N bytes
	// until a hard reload. Set an ETag that names the active gallery
	// so the conditional check (http.serveContent uses If-None-Match)
	// invalidates on a gallery switch even when mtimes happen to
	// match. no-cache forces revalidation on every visit so the
	// matching gallery still hits 304.
	if scaled && !gallery.IsVideoType(fileType) && fileType != "cbz" &&
		gallery.NeedsViewRendition(int(width.Int64), int(height.Int64)) {
		rendition, err := gallery.EnsureViewRendition(canonPath, cx.ThumbnailsPath, id)
		if err != nil {
			logx.Warnf("view rendition for image %s: %v", idStr, err)
		} else {
			setGalleryScopedCache(w, s.activeGallery(), idStr+"-view", rendition)
			w.Header().Set("Content-Type", "image/jpeg")
			http.ServeFile(w, r, rendition)
			return
		}
	}
	setGalleryScopedCache(w, s.activeGallery(), idStr, canonPath)
	if ct := gallery.MIMEForFileType(fileType); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Disposition", gallery.ContentDispositionFor(canonPath))
	http.ServeFile(w, r, canonPath)
}

// setGalleryScopedCache writes an ETag that includes the gallery name
// so a browser's cached copy from a different gallery's id=N is
// invalidated on the next conditional request. Falls back silently if
// the file can't be stat'd. The mtime is nanoseconds: a plugin turning
// an image twice inside one second writes two different files, and at
// whole-second resolution the second one rides the first one's tag and
// the browser keeps painting the bytes it already has.
func setGalleryScopedCache(w http.ResponseWriter, gallery, id, path string) {
	w.Header().Set("Cache-Control", "private, no-cache")
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%s-%s-%d"`, gallery, id, info.ModTime().UnixNano()))
}

// RestartRequested reports whether the shutdown under way should be followed
// by a fresh process. Read by the command once it has drained.
func (s *Server) RestartRequested() bool { return s.restart.Load() }

// QuitRequested fires when the Quit control asks the process to stop. The
// command selects on it beside the signal channel so both routes run the
// same graceful shutdown.
func (s *Server) QuitRequested() <-chan struct{} { return s.quit }

// Close stops background goroutines and closes every gallery's database.
func (s *Server) Close() {
	select {
	case <-s.done:
		// already closed
	default:
		close(s.done)
	}
	s.peers.StopAll()
	tagger.ReleaseAll()
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	for _, cx := range s.galleryState().contexts {
		cx.Close()
	}
}

// withConfig mutates the in-memory config under the write lock and persists it
// atomically, so a read-modify-write on a config slice can't lose a concurrent
// settings change. A non-nil fn error aborts the save.
func (s *Server) withConfig(fn func(*config.Config) error) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := fn(s.cfg); err != nil {
		return err
	}
	if err := config.Save(s.cfg, s.configPath); err != nil {
		logx.Errorf("config save: %v", err)
		return err
	}
	return nil
}

// saveConfig persists the config as it stands, for callers that have
// already made their edit. Returns any error so they can surface the
// failure instead of leaving the in-memory cfg out of sync with disk.
func (s *Server) saveConfig() error { return s.withConfig(func(*config.Config) error { return nil }) }
