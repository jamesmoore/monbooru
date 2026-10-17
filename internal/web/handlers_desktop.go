package web

import (
	"cmp"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tagger"
)

// The bind address is no condition: Restart must survive the wizard's network
// choice. Callers 404 to hide it.
func (s *Server) desktopLocal(r *http.Request) bool {
	return s.desktop && desktop.LocalRequest(r)
}

const maxBrowseEntries = 500

// The value lands in an attribute the picker's script reads: identifiers only.
var browseIntoRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type browseData struct {
	Into      string
	Path      string
	Parent    string
	Entries   []desktop.Dir
	Roots     []desktop.Dir
	Sandboxed bool
	Err       string
	Truncated bool
	Limit     int
}

func (s *Server) browseDirs(w http.ResponseWriter, r *http.Request) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
	into := r.URL.Query().Get("into")
	if !browseIntoRe.MatchString(into) {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	data := browseData{Into: into, Sandboxed: desktop.Sandboxed(), Limit: maxBrowseEntries}
	for _, root := range desktop.Roots() {
		data.Roots = append(data.Roots, desktop.Dir{Name: root, Path: root})
	}

	home, _ := os.UserHomeDir()
	path := cmp.Or(strings.TrimSpace(r.URL.Query().Get("path")), home, string(filepath.Separator))
	data.Path = filepath.Clean(path)
	if parent := filepath.Dir(data.Path); parent != data.Path {
		data.Parent = parent
	}

	var err error
	data.Entries, data.Truncated, err = desktop.ListDirs(data.Path, maxBrowseEntries)
	if err != nil {
		data.Err = "cannot read this folder"
	}
	s.renderTemplate(w, "partials/dir_picker.html", data)
}

// A kind, never a path, so no operator string reaches the launcher.
var openFolderKinds = []string{"config", "data", "gallery", "logs", "models"}

func (s *Server) openFolder(w http.ResponseWriter, r *http.Request) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	kind := r.FormValue("kind")
	if !slices.Contains(openFolderKinds, kind) {
		flashStatus(w, http.StatusBadRequest, "Unknown folder.")
		return
	}
	dir, ours := s.desktopFolder(kind)
	// A tagger name passes the allowlist, so it cannot leave the models folder.
	if kind == "models" && dir != "" {
		name := r.FormValue("name")
		if name != "" {
			if err := tagger.ValidateTaggerName(name); err != nil {
				flashStatus(w, http.StatusBadRequest, err.Error())
				return
			}
			dir = filepath.Join(dir, name)
		}
	}
	if dir == "" {
		flashStatus(w, http.StatusBadRequest, "That folder is not configured.")
		return
	}
	if ours {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			flashStatus(w, http.StatusInternalServerError, "Could not create "+dir+": "+err.Error())
			return
		}
	}
	if err := s.folderOpener(dir); err != nil {
		logx.Warnf("open-folder %s: %v", kind, err)
		flashStatus(w, http.StatusInternalServerError, "Could not open "+dir+".")
		return
	}
	writeInlineFlash(w, "ok", "Opened "+dir+".")
}

// The gallery is not ours to create: an empty one would be a second library.
func (s *Server) desktopFolder(kind string) (string, bool) {
	s.cfgMu.RLock()
	dataPath := s.cfg.Paths.DataPath
	modelPath := s.cfg.Paths.ModelPath
	s.cfgMu.RUnlock()
	switch kind {
	case "config":
		return filepath.Dir(s.configPath), true
	case "data":
		return dataPath, true
	case "logs":
		return s.logDir, true
	case "models":
		return modelPath, true
	case "gallery":
		if cx := s.active(); cx != nil {
			return cx.GalleryPath, false
		}
	}
	return "", false
}

func (s *Server) availableFolders() []string {
	out := make([]string, 0, len(openFolderKinds))
	for _, kind := range openFolderKinds {
		if dir, _ := s.desktopFolder(kind); dir != "" {
			out = append(out, kind)
		}
	}
	return out
}

func (s *Server) DesktopHook() desktop.Hook { return s.desktopHook() }

func (s *Server) desktopHook() desktop.Hook {
	icon, _ := fs.ReadFile(s.staticFS, "appicon.png")
	return desktop.Hook{
		App:        s.desktopApp,
		Name:       s.booruName(),
		Comment:    "Lightweight and fast private booru",
		Categories: "Graphics;2DGraphics;RasterGraphics;Viewer;",
		Icon:       icon,
	}
}

func (s *Server) desktopIntegration() desktop.Integration {
	return s.desktopHook().Integration(s.TrayEnabled())
}

func (s *Server) TrayEnabled() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Desktop.Tray
}

func applyDesktopHooks(h desktop.Hook, menu, autostart bool) []string {
	var failures []string
	if h.MenuSupported() {
		if err := h.SetMenu(menu); err != nil {
			failures = append(failures, "menu entry: "+err.Error())
		}
	}
	if h.AutostartSupported() {
		if err := h.SetAutostart(autostart); err != nil {
			failures = append(failures, "start at login: "+err.Error())
		}
	}
	return failures
}

func (s *Server) settingsDesktopPost(w http.ResponseWriter, r *http.Request) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	failures := applyDesktopHooks(s.desktopHook(), r.FormValue("menu_entry") == "on", r.FormValue("start_at_login") == "on")
	if desktop.TrayAvailable() {
		tray := r.FormValue("tray") == "on"
		if err := s.withConfig(func(c *config.Config) error {
			c.Desktop.Tray = tray
			return nil
		}); err != nil {
			failures = append(failures, "tray icon: "+err.Error())
		}
	}
	if len(failures) > 0 {
		flashStatus(w, http.StatusInternalServerError, strings.Join(failures, "; "))
		return
	}
	writeInlineFlash(w, "ok", "Saved.")
}

func (s *Server) desktopJobWarning() string {
	st := s.jobs.Get()
	if st == nil || !st.Running {
		return ""
	}
	return runningJobName(st.JobType) + " is running and will not resume."
}

// quitDelay lets the flushed page reach the browser before the listener closes.
const quitDelay = 300 * time.Millisecond

func (s *Server) settingsQuit(w http.ResponseWriter, r *http.Request) {
	s.stopAfterRender(w, r, map[string]any{
		"Heading": s.booruName() + " has stopped",
		"Hint":    "You can close this tab. Start it again from the applications menu.",
	}, "quit", s.RequestQuit)
}

func (s *Server) settingsRestart(w http.ResponseWriter, r *http.Request) {
	s.stopAfterRender(w, r, map[string]any{
		"Heading": s.booruName() + " is restarting",
		"Hint":    "This page will reload automatically.",
		"Poll":    true,
	}, "restart", s.requestRestart)
}

func (s *Server) stopAfterRender(w http.ResponseWriter, r *http.Request, page map[string]any, verb string, then func()) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
	heading, _ := page["Heading"].(string)
	s.renderTemplate(w, "message_page.html", s.standalonePageData(heading, page))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	logx.Infof("%s requested from the UI", verb)
	go func() {
		time.Sleep(quitDelay)
		then()
	}()
}

// Idempotent: the shutdown posts the tray's window close, which quits again.
func (s *Server) RequestQuit() {
	s.quitOnce.Do(func() { close(s.quit) })
}

func (s *Server) requestRestart() {
	s.restart.Store(true)
	s.RequestQuit()
}
