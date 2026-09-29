// Package web is the HTTP transport: routes, handlers and templates.
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
	"mime"
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

type tagGroup struct {
	Name  string
	Color string
	Tags  []models.Tag
}

type Server struct {
	cfg        *config.Config
	configPath string
	cfgMu      sync.RWMutex // guards cfg reads/writes and config.Save calls
	jobs       *jobs.Manager
	pairs      *pairStore
	sessions   *SessionStore
	loginRL    *loginRateLimiter
	csrfSecret []byte
	tmpl       *template.Template
	staticFS   fs.FS
	done       chan struct{}

	desktop bool
	// A field so a test can run the handler without an opener popping a window.
	folderOpener func(string) error
	logDir       string
	quit         chan struct{}
	quitOnce     sync.Once
	restart      atomic.Bool

	// ctxMu serialises gallery mutations and is read-held for a whole read
	// request, whose handler reads galState and never re-locks ctxMu: a nested
	// RLock queues behind a pending writer, which waits on the outer one.
	ctxMu    sync.RWMutex
	galState atomic.Pointer[galleryState]
	// Serialises rebuildBoundaries so an older rebuild cannot be stored last.
	boundsMu sync.Mutex

	sched *scheduler

	mlStatus *monloader.StatusCache

	themeWarn *themeWarnings

	peers *plugins.Peers

	fetchStatus *fetchStatusStore

	downloads downloadList
}

// Desktop.LogDir is not derived from the config: the command opens the
// log before the config loads, and data_path may point elsewhere.
type Desktop struct {
	Active bool
	LogDir string
}

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
	gallery.MetaTagsEnabled.Store(cfg.Gallery.AutoMetaTags)

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
	s.rebuildBoundaries()
	for i, a := range cfg.Galleries {
		for _, b := range cfg.Galleries[i+1:] {
			if gallery.SameFolder(a.GalleryPath, b.GalleryPath) {
				logx.Warnf("galleries %q and %q share the folder %q: each indexes all of it", a.Name, b.Name, a.GalleryPath)
			}
		}
		if err := s.inOwnFolder(a.GalleryPath); err != nil {
			logx.Warnf("gallery %q: %v", a.Name, err)
		}
	}

	if cfg.Server.ThemeColor != "" && !tags.IsValidCategoryColor(cfg.Server.ThemeColor) {
		logx.Warnf("server.theme_color %q is not a #rgb / #rrggbb colour; the bundled palette is used", cfg.Server.ThemeColor)
		s.cfg.Server.ThemeColor = ""
	}

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

	go s.runScheduler()

	go s.runPluginProbes()
	s.seedThemesDir()
	s.ensurePluginsDir()
	s.startManagedPlugins()

	return s, nil
}

// Long enough that a paused browse skips the rebuild, short enough that a
// finished one frees the memory.
const idleIndexReleaseAfter = 30 * time.Minute

// The short tick hands a job's peak working set back when the job ends,
// not at the next interval.
const (
	reclaimTick     = 30 * time.Second
	reclaimInterval = 5 * time.Minute
)

func reclaimDue(jobEnded bool, since time.Duration) bool { return jobEnded || since >= reclaimInterval }

// An export holds a read connection the shrink would wait on for the whole stream.
func shrinkGalleryMemory(cx *galleryCtx) error {
	if cx.Exporting() {
		return nil
	}
	return cx.DB.ShrinkMemory(context.Background())
}

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
				if err := shrinkGalleryMemory(cx); err != nil {
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

// Published values are immutable: writers hold ctxMu, clone, mutate the
// clone and store it.
type galleryState struct {
	contexts map[string]*galleryCtx
	active   string
}

func (st *galleryState) clone() *galleryState {
	next := &galleryState{contexts: make(map[string]*galleryCtx, len(st.contexts)+1), active: st.active}
	maps.Copy(next.contexts, st.contexts)
	return next
}

func (s *Server) galleryState() *galleryState { return s.galState.Load() }

func (s *Server) activeGallery() string { return s.galleryState().active }

func (s *Server) active() *galleryCtx {
	st := s.galleryState()
	return st.contexts[st.active]
}

func (s *Server) get(name string) *galleryCtx { return s.galleryState().contexts[name] }

type routeMode int

const (
	modeRead routeMode = iota
	// The handler takes ctxMu.Lock itself; a read lock taken at
	// registration would deadlock it.
	modeWrite
	// No lock: the handler must not keep a gallery context across a
	// window a mutation could close it in. For routes that read no
	// gallery, wait on a peer, or stream after a locked start.
	modeFree
)

// registerRoutes must not reach rt.mux directly: every route picks a mode
// through add.
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
		if pageGalleryStale(w, r, s.activeGallery()) {
			return
		}
		h(w, r)
	})
}

func (rt *routes) write(pattern string, h http.HandlerFunc) { rt.add(pattern, modeWrite, h) }

func (rt *routes) free(pattern string, h http.HandlerFunc) { rt.add(pattern, modeFree, h) }

// StartWatchers watches every gallery, not just the active one, and
// pre-warms each one's caches.
func (s *Server) StartWatchers() {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	for _, cx := range s.galleryState().contexts {
		cx.StartBackground(s.cfg.Gallery.WatchEnabled, s.cfg.Gallery.MaxFileSizeMB, s.ingestNaming(cx.Name), s.jobs)
		go cx.WarmCaches()
	}
}

func (s *Server) Handler() http.Handler {
	rt := s.newRoutes()
	s.registerRoutes(rt)

	// Outermost first: logging, session, first-run gate, CSRF.
	var h http.Handler = rt.mux
	h = s.cSRFMiddleware(h)
	h = s.setupMiddleware(h)
	h = s.sessionMiddleware(h)
	h = loggingMiddleware(h)

	return h
}

func (s *Server) registerRoutes(rt *routes) {
	// The thumbnail route needs no lock: it takes only the directory from
	// its context.
	rt.free("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.staticFS))).ServeHTTP)
	rt.free("GET /theme.css", s.serveThemeCSS)
	rt.free("GET /theme.logo", s.serveThemeLogo)
	rt.free("GET /theme.favicon", s.serveThemeFavicon)
	rt.free("GET /manifest.json", s.manifestHandler)
	rt.free("GET /thumbnails/{gallery}/{file}", s.serveThumbnail)
	// Found, not permanent: the target follows the active theme.
	rt.read("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.booruFaviconURL(), http.StatusFound)
	})

	rt.read("GET /health", func(w http.ResponseWriter, r *http.Request) {
		// Not refused over its Origin: the browser blocks a cross-origin
		// read itself, and a monitor sending one still gets an answer.
		api.SetCORS(w, r, s.cfgSnapshot())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// A second launch reads "app" to tell this instance from another
		// program on the port.
		_ = json.NewEncoder(w).Encode(map[string]string{"app": "monbooru", "status": "ok", "version": Version})
	})

	rt.read("GET /login", s.loginPage)
	rt.read("POST /login", s.loginPost)
	rt.read("POST /logout", s.logoutPost)

	rt.free("POST /upload", s.uploadPost)

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
	rt.read("POST /tags/{id}/note", s.setTagNote)
	rt.read("POST /tags/{id}/markup/preview", s.previewMarkup)
	// The /group suffix: a bare DELETE /tags/{id}/aliases would conflict
	// with DELETE /tags/categories/{id} in the mux.
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
	rt.read("POST /settings/general/ignore", s.settingsIgnorePost)
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
	rt.read("POST /settings/maintenance/meta-tags", s.generateMetaTagsPost)
	rt.read("POST /settings/maintenance/index-workflows", s.indexWorkflowsPost)
	rt.read("POST /settings/maintenance/meta-tags/remove", s.removeMetaTagsPost)
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
	rt.free("POST /internal/batch-download", s.batchDownload)
	rt.read("POST /internal/batch-download/count", s.batchDownloadCount)
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

	rt.write("POST /internal/gallery/switch", s.gallerySwitchHandler)
	rt.write("POST /settings/galleries", s.settingsGalleriesPost)
	rt.write("POST /settings/galleries/{name}/rename", s.settingsGalleryRenamePost)
	rt.write("POST /settings/galleries/{name}/delete", s.settingsGalleryDeletePost)
	rt.write("POST /settings/galleries/{name}/default", s.settingsGalleryDefaultPost)
	rt.free("GET /settings/galleries/{name}/export", s.settingsGalleryExport)
	rt.write("POST /settings/galleries/{name}/import", s.settingsGalleryImport)

	rt.read("POST /api/v1/pair/request", s.pairRequest)
	rt.read("GET /api/v1/pair/status", s.pairStatus)
	rt.read("POST /api/v1/pair/remove", s.pairTeardown)
	rt.read("GET /internal/plugins/pairing", s.pluginPairingFragment)
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
	rt.free("GET "+pluginMountPrefix+"{name}/", s.pluginMount)
	rt.free("POST "+pluginMountPrefix+"{name}/", s.pluginMount)

	// A mux of its own so the API subtree takes the read mode. The
	// methods are spelled out: a bare "/api/v1/" conflicts with "GET /".
	apiMux := http.NewServeMux()
	api.New(s.cfgSnapshot, s.jobs, s.apiResolver, Version).WithGalleryLock(func() func() {
		s.ctxMu.RLock()
		return s.ctxMu.RUnlock
	}).Mount(apiMux)
	// The exact pairing routes above answer ahead of this subtree, so
	// they skip notePeerAddress: none of them may move a peer.
	apiHandler := s.notePeerAddress(apiMux.ServeHTTP)
	// The uploads take the gallery lock themselves, once their body is in.
	rt.free("POST /api/v1/images", apiHandler)
	rt.free("POST /api/v1/images/{id}/file", apiHandler)
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"} {
		rt.read(method+" /api/v1/", apiHandler)
	}
}

func (s *Server) allContexts() []*galleryCtx {
	st := s.galleryState()
	out := make([]*galleryCtx, 0, len(st.contexts))
	for _, cx := range st.contexts {
		out = append(out, cx)
	}
	return out
}

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

func isNoisyPath(path string) bool {
	switch path {
	case "/internal/job/status", "/internal/monloader-status", "/health":
		return true
	}
	return strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/thumbnails/")
}

// Stamped by the outermost middleware so the footer's load time covers
// the whole chain.
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

// Version is set at build time via -ldflags.
var Version = "dev"

// RepoURL is set at build time via -ldflags.
var RepoURL = "https://github.com/monbooru/monbooru"

// DocURL is set at build time via -ldflags.
var DocURL = "https://monbooru.github.io/mondocs/index.html"

// Variant names the provider build, set via -ldflags; empty on the CPU build.
var Variant = ""

// Package names the artifact, set via -ldflags; "source" is a plain go build.
var Package = "source"

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

type ratingLevel struct {
	Value string
	Label string
}

var ratingFooterLevels = []ratingLevel{
	{Value: "general", Label: "sfw"},
	{Value: "sensitive", Label: "sensitive"},
	{Value: "questionable", Label: "questionable"},
	{Value: "explicit", Label: "explicit"},
}

type baseData struct {
	Title       string
	ActiveNav   string
	CSRFToken   string
	AuthEnabled bool
	Degraded    bool
	Version     string
	RepoURL     string
	DocURL      string
	Build       string
	Theme       bool
	// Server-rendered so a navigation does not flash the column before
	// main.js runs.
	SidebarCollapsed    bool
	BooruName           string
	BooruLogo           string
	BooruFavicon        string
	MonloaderURL        string
	ActiveGallery       string
	Galleries           []config.Gallery
	VisibleCount        int
	InboxCount          int
	TagCount            int
	CollectionsCount    int
	InboxNavActive      bool
	HiddenByCeiling     int
	RatingLevels        []ratingLevel
	ActiveRating        string
	RequestStart        time.Time
	MonloaderPaired     bool
	MonloaderUsable     bool
	MonloaderConn       string
	MonloaderVersion    string
	MonloaderPTR        bool
	MonloaderContrib    bool
	MonloaderPTRSyncing bool
	// The render gate, where MonloaderPTR is the live one: a paused or
	// unreachable link reports no PTR, so it counts as present.
	MonloaderPTRPresent bool
}

// main.js writes it; the name and the "collapsed" value must match.
const sidebarCookieName = "monbooru_sidebar"

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
		// Ceiling-aware: every surface showing it promises the count a
		// click will match.
		inbox, _ = cx.InboxCountUnder(resolveCeiling(r, cx))
		tagCount, _ = cx.TagCount()
		collectionsCount, _ = cx.CollectionsCount()
	}
	inboxNavActive := inboxFilterActive(search.Parse(r.URL.Query().Get("q")))
	galleries := s.galleries()
	active := readRatingCookie(r)
	active = cmp.Or(active, "explicit")
	ml := s.mlStatus.Seed()
	if s.monloaderPaused() {
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
	// Buffered so a failed execution can still send a clean 500.
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		logx.Errorf("template %q: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	if _, err := buf.WriteTo(w); err != nil {
		logx.Warnf("template %q write: %v", name, err)
	}
}

var thumbnailNameRe = regexp.MustCompile(`^\d+(?:_hover\.webp|\.jpg)$`)

// The gallery is in the URL so a switch cannot show another gallery's
// cached preview.
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
	// 204, not 404: the img onerror still fires, and the console logs no
	// error per card.
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// SQLite reuses the highest id once its row is deleted, so a
	// thumbnail URL can come back with new bytes.
	setGalleryScopedCache(w, r.PathValue("gallery"), file, fullPath)
	http.ServeFile(w, r, fullPath)
}

func (s *Server) serveConfiguredFile(w http.ResponseWriter, r *http.Request, path, kind string) {
	if path == "" {
		http.NotFound(w, r)
		return
	}
	setGalleryScopedCache(w, "custom", kind, path)
	http.ServeFile(w, r, path)
}

func (s *Server) booruName() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if name := s.cfg.Server.BooruName; name != "" {
		return name
	}
	return "Monbooru"
}

func (s *Server) modelPath() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Paths.ModelPath
}

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

const thumbSizeCookieName = "monbooru_thumb_size"

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

const pageSizeCookieName = "monbooru_page_size"

var PageSizeOptions = []int{20, 40, 60, 100, 250, 500}

// The cookie is client-writable, and the query budgets cover only the
// offered sizes.
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

// One read lock for both, so a save cannot land between them.
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

// The slices settings writers edit in place are cloned; what their elements
// hold is replaced wholesale, never written through, so one level is enough.
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

func (s *Server) galleries() []config.Gallery {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	out := make([]config.Gallery, len(s.cfg.Galleries))
	copy(out, s.cfg.Galleries)
	return out
}

func (s *Server) booruLogoURL() string {
	if s.activeTheme().Logo != "" {
		return "/theme.logo"
	}
	return "/static/logo.png"
}

// Versioned: browsers keep favicons outside the page cache and Firefox
// does not revalidate them, so a fixed URL keeps the first icon seen.
func (s *Server) booruFaviconURL() string {
	e := s.activeTheme()
	if e.Favicon == "" {
		return "/static/favicon.png"
	}
	return "/theme.favicon?v=" + themeFaviconVersion(e)
}

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

func (s *Server) monloaderWebBase() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	base := s.cfg.Server.MonloaderURL
	base = cmp.Or(base, s.cfg.Monloader.APIURL)
	return strings.TrimRight(base, "/")
}

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
	if !gallery.NamedInside(cx.GalleryPath, canonPath) {
		return "", false
	}
	return canonPath, true
}

func (s *Server) serveMangaPage(w http.ResponseWriter, r *http.Request) {
	s.serveMangaPagePath(w, r, gallery.EnsureMangaPage, "", true)
}

func (s *Server) serveMangaPageThumb(w http.ResponseWriter, r *http.Request) {
	s.serveMangaPagePath(w, r, gallery.EnsureMangaPageThumb, "-thumb", false)
}

func (s *Server) serveMangaPagePath(
	w http.ResponseWriter, r *http.Request,
	ensure func(thumbnailsPath, canonPath string, imageID int64, n int) (string, error),
	cacheSuffix string, negotiate bool,
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
	if fileType := gallery.ExtFileType(page); negotiate && gallery.IsFFmpegStill(fileType) {
		w.Header().Add("Vary", "Accept")
		if !acceptsMediaType(r.Header.Get("Accept"), gallery.MIMEForFileType(fileType)) {
			if view, err := gallery.EnsureMangaPageView(s.thumbnailsPath(), canonPath, id, n); err != nil {
				logx.Warnf("view copy for page %d of image %s: %v", n, idStr, err)
			} else {
				page, cacheSuffix = view, "-view"
			}
		}
	}
	// The archive's mtime, not the page's: every hit touches the page to keep
	// it cached, which would make each ETag new.
	setGalleryScopedCache(w, s.activeGallery(), fmt.Sprintf("%s-%d%s", idStr, n, cacheSuffix), canonPath)
	http.ServeFile(w, r, page)
}

func (s *Server) serveImageFile(w http.ResponseWriter, r *http.Request) {
	s.serveImageBytes(w, r, false)
}

// The viewers' URL; downloads and <video> stay on /file. A still past the
// display ceiling, or an AVIF or JPEG XL the browser does not accept, is
// served as a JPEG rendition.
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
	if !gallery.NamedInside(cx.GalleryPath, canonPath) {
		http.NotFound(w, r)
		return
	}
	if scaled && !gallery.IsVideoType(fileType) && fileType != "cbz" {
		past := gallery.NeedsViewRendition(int(width.Int64), int(height.Int64))
		// Not every browser decodes AVIF or JPEG XL: one that does names
		// the type in Accept, and any other gets a full-size JPEG.
		negotiated := gallery.IsFFmpegStill(fileType)
		if negotiated {
			w.Header().Add("Vary", "Accept")
		}
		if past || (negotiated && !acceptsMediaType(r.Header.Get("Accept"), gallery.MIMEForFileType(fileType))) {
			maxDim := 0
			if past {
				maxDim = gallery.ViewMaxDim
			}
			rendition, err := gallery.EnsureViewRendition(canonPath, cx.ThumbnailsPath, id, fileType, maxDim)
			if err != nil {
				logx.Warnf("view rendition for image %s: %v", idStr, err)
			} else {
				setGalleryScopedCache(w, s.activeGallery(), idStr+"-view", rendition)
				w.Header().Set("Content-Type", "image/jpeg")
				http.ServeFile(w, r, rendition)
				return
			}
		}
	}
	setGalleryScopedCache(w, s.activeGallery(), idStr, canonPath)
	if ct := gallery.MIMEForFileType(fileType); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Disposition", gallery.ContentDispositionFor(canonPath))
	http.ServeFile(w, r, canonPath)
}

// A wildcard does not count: every browser sends image/*, whether or not
// it decodes JPEG XL.
func acceptsMediaType(accept, mediaType string) bool {
	for _, part := range strings.Split(accept, ",") {
		t, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || t != mediaType {
			continue
		}
		q, err := strconv.ParseFloat(cmp.Or(params["q"], "1"), 64)
		return err == nil && q > 0
	}
	return false
}

// The gallery is in the tag because /images/{id} URLs repeat across galleries,
// and the mtime is in nanoseconds so two rewrites in a second differ.
func setGalleryScopedCache(w http.ResponseWriter, gallery, id, path string) {
	w.Header().Set("Cache-Control", "private, no-cache")
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%s-%s-%d"`, gallery, id, info.ModTime().UnixNano()))
}

func (s *Server) RestartRequested() bool { return s.restart.Load() }

// QuitRequested closes on a Quit or a Restart; RestartRequested tells
// them apart.
func (s *Server) QuitRequested() <-chan struct{} { return s.quit }

func (s *Server) Close() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	// Otherwise ReleaseAll waits out a running auto-tag chunk.
	s.jobs.Cancel()
	s.peers.StopAll()
	tagger.ReleaseAll()
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	for _, cx := range s.galleryState().contexts {
		cx.Close()
	}
}

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

func (s *Server) saveConfig() error { return s.withConfig(func(*config.Config) error { return nil }) }
