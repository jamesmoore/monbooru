package web

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/logx"
)

// Mutations hold the lane, not just check it: batch routes claim it under the
// ctxMu read lock, so a job can start between a check and the write lock.
var errJobRunning = errors.New("a job is running; try again when it finishes")

func exportRunning(cx *galleryCtx) error {
	if cx.Exporting() {
		return fmt.Errorf("gallery %q is being exported; try again when the export finishes", cx.Name)
	}
	return nil
}

func (s *Server) switchGallery(name string) error {
	if err := s.jobs.BeginSchedule(); err != nil {
		return errJobRunning
	}
	defer s.jobs.EndSchedule()
	s.ctxMu.Lock()
	st := s.galleryState()
	if name == st.active {
		s.ctxMu.Unlock()
		return nil
	}
	if _, ok := st.contexts[name]; !ok {
		s.ctxMu.Unlock()
		return fmt.Errorf("unknown gallery %q", name)
	}
	oldName := st.active
	next := st.clone()
	next.active = name
	s.galState.Store(next)
	s.ctxMu.Unlock()

	logx.Infof("gallery: switched from %q to %q", oldName, name)
	return nil
}

func (s *Server) setDefault(name string) error {
	s.ctxMu.Lock()
	if _, ok := s.galleryState().contexts[name]; !ok {
		s.ctxMu.Unlock()
		return fmt.Errorf("unknown gallery %q", name)
	}
	s.cfgMu.Lock()
	unchanged := s.cfg.DefaultGallery == name
	if !unchanged {
		s.cfg.DefaultGallery = name
	}
	s.cfgMu.Unlock()
	s.ctxMu.Unlock()
	if unchanged {
		return nil
	}

	if err := s.saveConfig(); err != nil {
		return fmt.Errorf("persist default gallery: %w", err)
	}
	logx.Infof("gallery: default set to %q", name)
	return nil
}

func (s *Server) rebuildBoundaries() {
	s.boundsMu.Lock()
	defer s.boundsMu.Unlock()
	s.cfgMu.RLock()
	ignore := slices.Clone(s.cfg.Gallery.Ignore)
	s.cfgMu.RUnlock()
	drawn := s.drawBoundaries(ignore)
	for name, cx := range s.galleryState().contexts {
		if b, ok := drawn[name]; ok {
			cx.Bounds.Store(b)
		}
	}
}

// Takes ignore rather than reading the config so a new list can be
// checked before it is stored.
func (s *Server) drawBoundaries(ignore []string) map[string]*gallery.Boundary {
	s.cfgMu.RLock()
	galleries := slices.Clone(s.cfg.Galleries)
	own := []gallery.Fence{
		{Path: s.cfg.Paths.DataPath, Kind: gallery.FenceData},
		{Path: s.cfg.Paths.ModelPath, Kind: gallery.FenceModels},
	}
	s.cfgMu.RUnlock()
	own = append(own,
		gallery.Fence{Path: s.themesDir(), Kind: gallery.FenceThemes},
		gallery.Fence{Path: s.pluginsDir(), Kind: gallery.FencePlugins})
	// A gallery mounted under data_path is not fenced off by it, but what
	// monbooru writes there still has to stay out of that gallery.
	for _, g := range galleries {
		own = append(own,
			gallery.Fence{Path: g.DBPath, Kind: gallery.FenceData},
			gallery.Fence{Path: g.DBPath + "-wal", Kind: gallery.FenceData},
			gallery.Fence{Path: g.DBPath + "-shm", Kind: gallery.FenceData},
			gallery.Fence{Path: g.ThumbnailsPath, Kind: gallery.FenceData},
			gallery.Fence{Path: gallery.MangaCacheDir(g.ThumbnailsPath), Kind: gallery.FenceData})
	}
	drawn := map[string]*gallery.Boundary{}
	for name, cx := range s.galleryState().contexts {
		fences := slices.Clone(own)
		for _, g := range galleries {
			if g.Name != name {
				fences = append(fences, gallery.Fence{Path: g.GalleryPath, Owner: g.Name, Kind: gallery.FenceGallery})
			}
		}
		drawn[name] = gallery.NewBoundary(cx.GalleryPath, fences, ignore)
	}
	return drawn
}

// Nothing watched dir while it was fenced off. The caller holds the job
// lane rather than ctxMu: a restart walks the whole tree.
func (s *Server) restartEnclosing(dir string) {
	watch, maxMB := s.watcherSettings()
	for _, cx := range s.galleryState().contexts {
		if gallery.Encloses(cx.GalleryPath, dir) {
			cx.RestartWatcher(watch, maxMB, s.ingestNaming(cx.Name), s.jobs)
		}
	}
}

func (s *Server) addGallery(name, galleryPath string) error {
	name = strings.TrimSpace(name)
	galleryPath = strings.TrimSpace(galleryPath)
	if err := config.ValidateGalleryName(name); err != nil {
		return err
	}
	if galleryPath == "" {
		return fmt.Errorf("gallery path must not be empty")
	}
	// Only an add refuses an unreadable folder; a configured one still
	// loads, degraded.
	if _, err := os.ReadDir(galleryPath); err != nil {
		return fmt.Errorf("gallery path %q is not readable: %w", galleryPath, err)
	}
	if err := s.jobs.BeginSchedule(); err != nil {
		return errJobRunning
	}
	defer s.jobs.EndSchedule()

	s.ctxMu.Lock()
	next := s.galleryState().clone()
	if _, ok := next.contexts[name]; ok {
		s.ctxMu.Unlock()
		return fmt.Errorf("gallery %q already exists", name)
	}
	if other := s.galleryOnFolder(galleryPath, ""); other != "" {
		s.ctxMu.Unlock()
		return fmt.Errorf("%s is already the folder of gallery %s", galleryPath, other)
	}
	if err := s.inOwnFolder(galleryPath, name); err != nil {
		s.ctxMu.Unlock()
		return err
	}
	if dir, what := s.dataFolderHolds(name); what != "" {
		s.ctxMu.Unlock()
		return fmt.Errorf("a gallery named %s would keep its data in %s, which holds %s", name, dir, what)
	}
	dbPath, thumbnailsPath := s.derivePaths(name)
	if _, err := os.Stat(dbPath); err == nil {
		s.ctxMu.Unlock()
		return fmt.Errorf("data for gallery %q already exists at %q", name, filepath.Dir(dbPath))
	}
	g := config.Gallery{
		Name:           name,
		GalleryPath:    galleryPath,
		DBPath:         dbPath,
		ThumbnailsPath: thumbnailsPath,
	}
	cx, err := library.Open(g)
	if err != nil {
		s.ctxMu.Unlock()
		return err
	}
	next.contexts[name] = cx
	s.galState.Store(next)
	s.cfgMu.Lock()
	s.cfg.Galleries = append(s.cfg.Galleries, g)
	s.cfgMu.Unlock()
	s.rebuildBoundaries()
	watch, maxMB := s.watcherSettings()
	cx.StartBackground(watch, maxMB, s.ingestNaming(cx.Name), s.jobs)
	s.ctxMu.Unlock()

	if err := s.saveConfig(); err != nil {
		return fmt.Errorf("persist new gallery: %w", err)
	}
	logx.Infof("gallery: added %q (path=%q)", name, galleryPath)
	return nil
}

func (s *Server) removeGallery(name string, removeFolder bool) (kept []string, err error) {
	if err := s.jobs.BeginSchedule(); err != nil {
		return nil, errJobRunning
	}
	defer s.jobs.EndSchedule()
	s.ctxMu.Lock()
	next := s.galleryState().clone()
	cx, ok := next.contexts[name]
	if !ok {
		s.ctxMu.Unlock()
		return nil, fmt.Errorf("unknown gallery %q", name)
	}
	if name == next.active {
		s.ctxMu.Unlock()
		return nil, fmt.Errorf("cannot remove the active gallery; switch to another first")
	}
	if name == s.defaultGallery() {
		s.ctxMu.Unlock()
		return nil, fmt.Errorf("cannot remove the default gallery; set another as default first")
	}
	if len(next.contexts) <= 1 {
		s.ctxMu.Unlock()
		return nil, fmt.Errorf("cannot remove the last gallery")
	}
	if err := exportRunning(cx); err != nil {
		s.ctxMu.Unlock()
		return nil, err
	}

	galleryPath := cx.GalleryPath
	bound := cx.Boundary()
	dataDir := filepath.Dir(cx.DBPath)
	owned := []string{cx.DBPath, cx.DBPath + "-wal", cx.DBPath + "-shm", cx.ThumbnailsPath, cx.MangaCacheDir()}
	cx.Close()
	delete(next.contexts, name)
	s.galState.Store(next)
	s.cfgMu.Lock()
	s.cfg.Galleries = slices.DeleteFunc(s.cfg.Galleries, func(g config.Gallery) bool { return g.Name == name })
	s.cfg.DropGalleryRefs(name)
	s.cfgMu.Unlock()
	s.rebuildBoundaries()
	s.ctxMu.Unlock()

	// Only what monbooru wrote: a gallery folder can sit in this directory.
	for _, p := range owned {
		if err := os.RemoveAll(p); err != nil {
			logx.Warnf("remove gallery data %q: %v", p, err)
		}
	}
	if removeFolder {
		// Not through a symlink: the link could resolve to any directory.
		if info, err := os.Lstat(galleryPath); err != nil {
			logx.Warnf("remove gallery folder %q: stat: %v", galleryPath, err)
		} else if info.Mode()&os.ModeSymlink != 0 {
			logx.Warnf("remove gallery folder %q: refusing to follow symlink", galleryPath)
		} else if kept, err = gallery.RemoveOwned(bound); err != nil {
			logx.Warnf("remove gallery folder %q: %v", galleryPath, err)
		} else if len(kept) > 0 {
			// Other galleries' folders go first: what the ignore list
			// kept can be a sidecar per image.
			fenced := map[string]bool{}
			for _, e := range bound.Fenced() {
				fenced[e.Rel] = true
			}
			var first, rest []string
			for _, k := range kept {
				if fenced[k] {
					first = append(first, k)
				} else {
					rest = append(rest, k)
				}
			}
			kept = append(first, rest...)
			logx.Infof("remove gallery folder %q: kept %s", galleryPath, listFirst(kept, keptShown))
		} else if err := os.Remove(galleryPath); err != nil {
			logx.Warnf("remove gallery folder %q: %v", galleryPath, err)
		}
	}
	if err := os.Remove(dataDir); err != nil && !os.IsNotExist(err) {
		logx.Infof("gallery data dir %q kept: %v", dataDir, err)
	}
	s.restartEnclosing(galleryPath)

	if err := s.saveConfig(); err != nil {
		return kept, fmt.Errorf("persist gallery removal: %w", err)
	}
	logx.Infof("gallery: removed %q (folder removed=%t)", name, removeFolder)
	return kept, nil
}

func (s *Server) renameGallery(oldName, newName string) error {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == newName {
		return nil
	}
	if err := config.ValidateGalleryName(newName); err != nil {
		return err
	}
	if err := s.jobs.BeginSchedule(); err != nil {
		return errJobRunning
	}
	defer s.jobs.EndSchedule()
	s.ctxMu.Lock()
	next := s.galleryState().clone()
	cx, ok := next.contexts[oldName]
	if !ok {
		s.ctxMu.Unlock()
		return fmt.Errorf("unknown gallery %q", oldName)
	}
	if _, exists := next.contexts[newName]; exists {
		s.ctxMu.Unlock()
		return fmt.Errorf("gallery %q already exists", newName)
	}
	if dir, what := s.dataFolderHolds(oldName); what != "" {
		s.ctxMu.Unlock()
		return fmt.Errorf("%s holds %s, which a rename would move; move it out first", dir, what)
	}
	newDB, newThumbs := s.derivePaths(newName)
	newDir := filepath.Dir(newDB)
	if _, err := os.Stat(newDir); err == nil {
		s.ctxMu.Unlock()
		return fmt.Errorf("data dir %q already exists", newDir)
	}
	if err := exportRunning(cx); err != nil {
		s.ctxMu.Unlock()
		return err
	}
	cx.Close()
	oldDir := filepath.Dir(cx.DBPath)
	restoreOld := func() {
		reopened, err := library.Open(config.Gallery{
			Name: oldName, GalleryPath: cx.GalleryPath, DBPath: cx.DBPath, ThumbnailsPath: cx.ThumbnailsPath,
		})
		if err != nil {
			logx.Errorf("gallery %q: could not reopen after a refused rename: %v", oldName, err)
			return
		}
		next.contexts[oldName] = reopened
		s.galState.Store(next)
		s.rebuildBoundaries()
		watch, maxMB := s.watcherSettings()
		reopened.StartBackground(watch, maxMB, s.ingestNaming(oldName), s.jobs)
	}
	moved := false
	if err := os.Rename(oldDir, newDir); err != nil {
		if !os.IsNotExist(err) {
			restoreOld()
			s.ctxMu.Unlock()
			return fmt.Errorf("rename data dir %q -> %q: %w", oldDir, newDir, err)
		}
	} else {
		moved = true
	}
	moveBack := func() {
		if !moved {
			return
		}
		if err := os.Rename(newDir, oldDir); err != nil {
			logx.Errorf("gallery %q: could not put the data dir back at %q: %v", oldName, oldDir, err)
		}
	}
	newCx, err := library.Open(config.Gallery{
		Name: newName, GalleryPath: cx.GalleryPath, DBPath: newDB, ThumbnailsPath: newThumbs,
	})
	if err != nil {
		moveBack()
		restoreOld()
		s.ctxMu.Unlock()
		return err
	}
	// Saved before the new name goes live: a restart on a config that still
	// names the old gallery would open it empty and orphan the moved data.
	s.cfgMu.Lock()
	undoCfg := renameGalleryInConfig(s.cfg, oldName, newName, newDB, newThumbs)
	err = config.Save(s.cfg, s.configPath)
	if err != nil {
		undoCfg()
	}
	s.cfgMu.Unlock()
	if err != nil {
		newCx.Close()
		moveBack()
		restoreOld()
		s.ctxMu.Unlock()
		return fmt.Errorf("persist gallery rename: %w", err)
	}
	delete(next.contexts, oldName)
	next.contexts[newName] = newCx
	if next.active == oldName {
		next.active = newName
	}
	s.galState.Store(next)
	s.rebuildBoundaries()
	watch, maxMB := s.watcherSettings()
	newCx.StartBackground(watch, maxMB, s.ingestNaming(newCx.Name), s.jobs)
	s.ctxMu.Unlock()

	logx.Infof("gallery: renamed %q to %q", oldName, newName)
	return nil
}

func renameGalleryInConfig(cfg *config.Config, oldName, newName, newDB, newThumbs string) func() {
	galleries, def, schedule := slices.Clone(cfg.Galleries), cfg.DefaultGallery, cfg.Schedule.Galleries
	taggerLists := make([][]string, len(cfg.Tagger.Taggers))
	for i := range cfg.Tagger.Taggers {
		taggerLists[i] = cfg.Tagger.Taggers[i].Galleries
	}
	for i := range cfg.Galleries {
		if cfg.Galleries[i].Name == oldName {
			cfg.Galleries[i].Name = newName
			cfg.Galleries[i].DBPath = newDB
			cfg.Galleries[i].ThumbnailsPath = newThumbs
			break
		}
	}
	if cfg.DefaultGallery == oldName {
		cfg.DefaultGallery = newName
	}
	cfg.RenameGalleryRefs(oldName, newName)
	return func() {
		cfg.Galleries, cfg.DefaultGallery, cfg.Schedule.Galleries = galleries, def, schedule
		for i := range cfg.Tagger.Taggers {
			cfg.Tagger.Taggers[i].Galleries = taggerLists[i]
		}
	}
}

// A reopen, not an edit in place: the context caches the path and the
// watcher holds it open.
func (s *Server) repointGallery(name, galleryPath string) error {
	galleryPath = filepath.Clean(strings.TrimSpace(galleryPath))
	if galleryPath == "" || !filepath.IsAbs(galleryPath) {
		return fmt.Errorf("the gallery folder must be an absolute path")
	}
	if _, err := os.ReadDir(galleryPath); err != nil {
		return fmt.Errorf("gallery path %q is not readable: %w", galleryPath, err)
	}
	if err := s.jobs.BeginSchedule(); err != nil {
		return errJobRunning
	}
	defer s.jobs.EndSchedule()
	s.ctxMu.Lock()
	next := s.galleryState().clone()
	cx, ok := next.contexts[name]
	if !ok {
		s.ctxMu.Unlock()
		return fmt.Errorf("unknown gallery %q", name)
	}
	if cx.GalleryPath == galleryPath {
		s.ctxMu.Unlock()
		return nil
	}
	if other := s.galleryOnFolder(galleryPath, name); other != "" {
		s.ctxMu.Unlock()
		return fmt.Errorf("%s is already the folder of gallery %s", galleryPath, other)
	}
	if err := s.inOwnFolder(galleryPath); err != nil {
		s.ctxMu.Unlock()
		return err
	}
	if err := exportRunning(cx); err != nil {
		s.ctxMu.Unlock()
		return err
	}
	// Opened before the old one closes, so a failed open leaves the
	// gallery as it was.
	newCx, err := library.Open(config.Gallery{
		Name: name, GalleryPath: galleryPath, DBPath: cx.DBPath, ThumbnailsPath: cx.ThumbnailsPath,
	})
	if err != nil {
		s.ctxMu.Unlock()
		return err
	}
	oldPath := cx.GalleryPath
	cx.Close()
	next.contexts[name] = newCx
	s.galState.Store(next)
	s.cfgMu.Lock()
	if g := s.cfg.FindGallery(name); g != nil {
		g.GalleryPath = galleryPath
	}
	s.cfgMu.Unlock()
	s.rebuildBoundaries()
	watch, maxMB := s.watcherSettings()
	newCx.StartBackground(watch, maxMB, s.ingestNaming(name), s.jobs)
	s.ctxMu.Unlock()
	s.restartEnclosing(oldPath)

	if err := s.saveConfig(); err != nil {
		return fmt.Errorf("persist gallery path: %w", err)
	}
	logx.Infof("gallery: %q now points at %q", name, galleryPath)
	return nil
}

func (s *Server) galleryList() []config.Gallery {
	out := s.galleries()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type galleryRow struct {
	config.Gallery
	Images    int
	Tags      int
	LeavesOut []string
}

// The active row takes the caller's counts, which the footer shows: a
// second cache read could straddle an invalidation and disagree.
func (s *Server) galleryRowsWithSnapshot(activeName string, activeImages, activeTags int) []galleryRow {
	galleries := s.galleryList()
	out := make([]galleryRow, len(galleries))
	for i, g := range galleries {
		out[i].Gallery = g
		cx := s.get(g.Name)
		if cx != nil {
			for _, e := range cx.Boundary().Fenced() {
				out[i].LeavesOut = append(out[i].LeavesOut, e.Rel+" ("+e.OwnerName()+")")
			}
		}
		if g.Name == activeName {
			out[i].Images = activeImages
			out[i].Tags = activeTags
			continue
		}
		if cx == nil || cx.DB == nil {
			continue
		}
		if n, ok := cx.VisibleCount(); ok {
			out[i].Images = n
		}
		if n, ok := cx.TagCount(); ok {
			out[i].Tags = n
		}
	}
	return out
}

func (s *Server) gallerySwitchHandler(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if err := s.switchGallery(name); err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		// Home, not a refresh: the ids in the current URL mean other rows
		// in the new gallery.
		w.Header().Set("HX-Redirect", "/")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) settingsGalleriesPost(w http.ResponseWriter, r *http.Request) {
	const maxImport = 16 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxImport)
	mr, err := r.MultipartReader()
	if err != nil {
		writeInlineFlash(w, "err", "bad form data: "+err.Error())
		return
	}
	fields, filePart, err := readFieldsToFile(mr)
	if err != nil {
		writeInlineFlash(w, "err", "bad form data: "+err.Error())
		return
	}
	if filePart != nil {
		defer func() { _ = filePart.Close() }()
	}
	name := fields["name"]
	if err := s.addGallery(name, fields["gallery_path"]); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}

	nesting := s.nestingNote(name)
	var file *bufio.Reader
	if filePart != nil {
		file = bufio.NewReader(filePart)
		if _, err := file.Peek(1); err == io.EOF {
			file = nil
		}
	}
	if file == nil {
		if switchErr := s.switchGallery(name); switchErr != nil {
			logx.Infof("gallery %q: post-add switch skipped: %v", name, switchErr)
		}
		writeInlineFlash(w, "ok", "Gallery "+name+" added and now active."+nesting)
		return
	}
	format := galleryio.FormatFromExt(filePart.FileName())
	if format == "" {
		writeInlineFlash(w, "err", "Gallery created. Import failed: file must be .db, .json, or .zip.")
		return
	}
	leftOut, err := s.importGallery(name, format, file)
	if err != nil {
		writeInlineFlash(w, "err", "Gallery created. Import failed: "+err.Error())
		return
	}
	writeInlineFlash(w, "ok", "Gallery "+name+" added and imported."+nesting+leftOutNote(leftOut))
}

const keptShown = 5

func listFirst(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s (and %d more)", strings.Join(names[:n], ", "), len(names)-n)
}

func (s *Server) galleryOnFolder(folder, skip string) string {
	for _, g := range s.galleries() {
		if g.Name != skip && gallery.SameFolder(g.GalleryPath, folder) {
			return g.Name
		}
	}
	return ""
}

type ownFolder struct{ path, what string }

func (s *Server) monbooruFolders() []ownFolder {
	s.cfgMu.RLock()
	model := s.cfg.Paths.ModelPath
	s.cfgMu.RUnlock()
	return []ownFolder{
		{model, "monbooru's model folder"},
		{s.themesDir(), "monbooru's theme folder"},
		{s.pluginsDir(), "monbooru's plugin folder"},
	}
}

func holds(outer, inner string) bool {
	return outer != "" && inner != "" && (gallery.SameFolder(outer, inner) || gallery.Encloses(outer, inner))
}

// A gallery folder inside a folder monbooru writes to loses its files to
// that folder's cleanup, or moves with it on a rename. names are galleries
// not configured yet, whose data folders count too.
func (s *Server) inOwnFolder(galleryPath string, names ...string) error {
	s.cfgMu.RLock()
	dataPath := s.cfg.Paths.DataPath
	folders := make([]ownFolder, 0, len(s.cfg.Galleries)+len(names))
	for _, g := range s.cfg.Galleries {
		folders = append(folders, ownFolder{filepath.Dir(g.DBPath), "the data folder of gallery " + g.Name})
	}
	for _, name := range names {
		db, _ := s.cfg.DerivePaths(name)
		folders = append(folders, ownFolder{filepath.Dir(db), "the data folder of gallery " + name})
	}
	s.cfgMu.RUnlock()
	if gallery.SameFolder(galleryPath, dataPath) {
		return fmt.Errorf("%s is monbooru's data folder; pick a folder outside it", galleryPath)
	}
	for _, f := range append(folders, s.monbooruFolders()...) {
		if holds(f.path, galleryPath) {
			return fmt.Errorf("%s is inside %s; pick a folder outside it", galleryPath, f.what)
		}
	}
	return nil
}

func (s *Server) dataFolderHolds(name string) (dir, what string) {
	db, _ := s.derivePaths(name)
	dir = filepath.Dir(db)
	held := s.monbooruFolders()
	for _, g := range s.galleries() {
		held = append(held, ownFolder{g.GalleryPath, "the folder of gallery " + g.Name})
	}
	for _, h := range held {
		if holds(dir, h.path) {
			return dir, h.what
		}
	}
	return dir, ""
}

func (s *Server) nestingNote(name string) string {
	cx := s.get(name)
	if cx == nil {
		return ""
	}
	var around *galleryCtx
	var inside []config.Gallery
	for _, g := range s.galleryList() {
		other := s.get(g.Name)
		if g.Name == name || other == nil {
			continue
		}
		switch {
		case gallery.Encloses(g.GalleryPath, cx.GalleryPath):
			if around == nil || gallery.Encloses(around.GalleryPath, g.GalleryPath) {
				around = other
			}
		case gallery.Encloses(cx.GalleryPath, g.GalleryPath):
			inside = append(inside, g)
		}
	}
	var outermost []string
	for _, g := range inside {
		if !slices.ContainsFunc(inside, func(o config.Gallery) bool { return gallery.Encloses(o.GalleryPath, g.GalleryPath) }) {
			outermost = append(outermost, g.Name)
		}
	}
	note := ""
	if around != nil {
		note += fmt.Sprintf(" It sits inside gallery %s, which leaves this folder to it from now on", around.Name)
		if n := imagesUnder(around, name); n > 0 {
			note += fmt.Sprintf(": %s's next sync marks the %d image(s) it holds there missing", around.Name, n)
		}
		note += "."
	}
	switch len(outermost) {
	case 0:
	case 1:
		note += fmt.Sprintf(" It leaves out the folder of gallery %s.", outermost[0])
	default:
		note += fmt.Sprintf(" It leaves out the folders of galleries %s.", strings.Join(outermost, ", "))
	}
	return note
}

func imagesUnder(cx *galleryCtx, inner string) int {
	for _, e := range cx.Boundary().Fenced() {
		if e.Kind != gallery.FenceGallery || e.Owner != inner {
			continue
		}
		var n int
		if err := cx.DB.Read.QueryRow(
			`SELECT COUNT(*) FROM images WHERE is_missing = 0
			   AND (folder_path = ? COLLATE NOCASE OR folder_path LIKE ? ESCAPE '\' COLLATE NOCASE)`,
			e.Rel, db.EscapeLike(e.Rel)+"/%",
		).Scan(&n); err != nil {
			logx.Warnf("gallery %q: count images under %q: %v", cx.Name, e.Rel, err)
		}
		return n
	}
	return 0
}

func (s *Server) settingsGalleryRenamePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	oldName := r.PathValue("name")
	newName := strings.TrimSpace(r.FormValue("new_name"))
	if err := s.renameGallery(oldName, newName); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	if oldName != newName {
		if err := s.monloaderGalleryRenamed(r.Context(), oldName, newName); err != nil {
			logx.Warnf("gallery: telling monloader about the rename of %q: %v", oldName, err)
			writeInlineFlash(w, "warn", fmt.Sprintf("Renamed %s to %s. monloader could not be told, so its default gallery and site targets still name %s.", oldName, newName, oldName))
			return
		}
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) settingsGalleryDeletePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	confirm := strings.TrimSpace(r.FormValue("confirm_name"))
	if confirm != name {
		writeInlineFlash(w, "err", "type-to-confirm name does not match")
		return
	}
	removeFolder := r.FormValue("remove_folder") == "on"
	kept, err := s.removeGallery(name, removeFolder)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	if len(kept) > 0 {
		writeInlineFlash(w, "ok", fmt.Sprintf("Deleted %s. Kept what it leaves out: %s.", name, listFirst(kept, keptShown)))
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) settingsGalleryDefaultPost(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.setDefault(name); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	w.Header().Set("HX-Refresh", "true")
}
