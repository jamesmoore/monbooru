package web

import (
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/desktop"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tagger"
)

// The bind address is no condition: Restart must survive the wizard's
// network choice. A forwarded hop is refused: behind a same-host proxy
// every peer is loopback. Callers 404 so nothing is advertised.
func (s *Server) desktopLocal(r *http.Request) bool {
	if !s.desktop {
		return false
	}
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

const maxBrowseEntries = 500

// The value lands in an attribute the picker's script reads: identifiers only.
var browseIntoRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type browseEntry struct {
	Name string
	Path string
}

type browseData struct {
	Into      string
	Path      string
	Parent    string
	Entries   []browseEntry
	Roots     []browseEntry
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
		data.Roots = append(data.Roots, browseEntry{Name: root, Path: root})
	}

	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		path, _ = os.UserHomeDir()
	}
	if path == "" {
		path = string(filepath.Separator)
	}
	data.Path = filepath.Clean(path)
	if parent := filepath.Dir(data.Path); parent != data.Path {
		data.Parent = parent
	}

	entries, err := os.ReadDir(data.Path)
	if err != nil {
		data.Err = "cannot read this folder"
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(data.Path, e.Name())
		// Only a symlink needs a stat; the dirent answers for a real directory.
		if !e.IsDir() {
			if e.Type()&fs.ModeSymlink == 0 {
				continue
			}
			if fi, err := os.Stat(full); err != nil || !fi.IsDir() {
				continue
			}
		}
		data.Entries = append(data.Entries, browseEntry{Name: e.Name(), Path: full})
	}
	// Sorted before the cap, so the listing is the folder's alphabetical head.
	slices.SortFunc(data.Entries, func(a, b browseEntry) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	if len(data.Entries) > maxBrowseEntries {
		data.Entries = data.Entries[:maxBrowseEntries]
		data.Truncated = true
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
		App:     "monbooru",
		Name:    s.booruName(),
		Comment: "Lightweight and fast private booru",
		Icon:    icon,
	}
}

// Menu and autostart are read from disk so a hand-removed file shows as off.
type desktopIntegration struct {
	MenuSupported      bool
	MenuEnabled        bool
	AutostartSupported bool
	AutostartEnabled   bool
	TraySupported      bool
	TrayEnabled        bool
}

func (d desktopIntegration) Supported() bool { return d.MenuSupported || d.AutostartSupported }

func (s *Server) desktopIntegration() desktopIntegration {
	h := s.desktopHook()
	return desktopIntegration{
		MenuSupported:      h.MenuSupported(),
		MenuEnabled:        h.MenuEnabled(),
		AutostartSupported: h.AutostartSupported(),
		AutostartEnabled:   h.AutostartEnabled(),
		TraySupported:      desktop.TrayAvailable(),
		TrayEnabled:        s.TrayEnabled(),
	}
}

func (s *Server) TrayEnabled() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Desktop.Tray
}

func (s *Server) settingsDesktopPost(w http.ResponseWriter, r *http.Request) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	h := s.desktopHook()
	var failures []string
	if h.MenuSupported() {
		if err := h.SetMenu(r.FormValue("menu_entry") == "on"); err != nil {
			failures = append(failures, "menu entry: "+err.Error())
		}
	}
	if h.AutostartSupported() {
		if err := h.SetAutostart(r.FormValue("start_at_login") == "on"); err != nil {
			failures = append(failures, "start at login: "+err.Error())
		}
	}
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
