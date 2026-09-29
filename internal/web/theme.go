package web

import (
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
	webFS "github.com/monbooru/monbooru/web"
)

// Path, Logo and Favicon are disk paths, or embedded-FS paths for a
// built-in; the dark built-in has no Path because it is main.css.
type themeEntry struct {
	Name    string
	Path    string
	Logo    string
	Favicon string
	Builtin bool
}

const builtinLightDir = "static/themes/light"

var builtinThemes = []themeEntry{
	{Name: "dark", Builtin: true},
	{
		Name: "light", Builtin: true,
		Path: builtinLightDir + "/theme.css",
		Logo: builtinLightDir + "/logo.png",
	},
}

func (s *Server) themesDir() string { return s.configSubdir("themes") }

// After the first start it adds only the files the copy lacks, never
// rewriting one: the copy shadows the built-in, so a file a release adds
// would otherwise reach new installs only. A deleted copy is not rebuilt.
func (s *Server) seedThemesDir() {
	dir := s.themesDir()
	if dir == "" {
		return
	}
	light := filepath.Join(dir, "light")
	if _, err := os.Stat(light); err != nil {
		if _, err := os.Stat(dir); err == nil {
			return
		}
		if err := os.MkdirAll(light, 0o755); err != nil {
			logx.Warnf("themes: could not create %s: %v", dir, err)
			return
		}
	}
	for _, name := range []string{"theme.css", "logo.png"} {
		path := filepath.Join(light, name)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		body, err := webFS.FS.ReadFile(builtinLightDir + "/" + name)
		if err != nil {
			continue
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			logx.Warnf("themes: could not write the example theme: %v", err)
		}
	}
}

func (s *Server) listThemes() []themeEntry {
	byName := map[string]themeEntry{}
	for _, e := range builtinThemes {
		byName[e.Name] = e
	}
	dir := s.themesDir()
	if dir == "" {
		return orderThemes(byName)
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		return orderThemes(byName)
	}
	for _, it := range items {
		if it.IsDir() {
			sheet := filepath.Join(dir, it.Name(), "theme.css")
			if _, err := os.Stat(sheet); err != nil {
				continue
			}
			e := themeEntry{Name: it.Name(), Path: sheet}
			optional := func(file string) string {
				p := filepath.Join(dir, it.Name(), file)
				if _, err := os.Stat(p); err != nil {
					return ""
				}
				return p
			}
			e.Logo, e.Favicon = optional("logo.png"), optional("favicon.png")
			byName[e.Name] = e
			continue
		}
		if !strings.HasSuffix(it.Name(), ".css") {
			continue
		}
		name := strings.TrimSuffix(it.Name(), ".css")
		if prev, taken := byName[name]; taken && !prev.Builtin {
			logx.Warnf("themes: %q exists as both a folder and a .css file; the folder wins", name)
			continue
		}
		byName[name] = themeEntry{Name: name, Path: filepath.Join(dir, it.Name())}
	}
	return orderThemes(byName)
}

func orderThemes(byName map[string]themeEntry) []themeEntry {
	out := make([]themeEntry, 0, len(byName))
	for _, b := range builtinThemes {
		if e, ok := byName[b.Name]; ok {
			out = append(out, e)
			delete(byName, b.Name)
		}
	}
	rest := slices.SortedFunc(maps.Values(byName), func(a, b themeEntry) int {
		return strings.Compare(a.Name, b.Name)
	})
	return append(out, rest...)
}

// server.theme is only matched against the listing, never used as a path.
func (s *Server) activeTheme() themeEntry {
	s.cfgMu.RLock()
	name := strings.TrimSpace(s.cfg.Server.Theme)
	s.cfgMu.RUnlock()
	if name == "" {
		return builtinThemes[0]
	}
	for _, e := range s.listThemes() {
		if e.Name == name {
			return e
		}
	}
	s.themeWarn.warnOnce(name)
	return builtinThemes[0]
}

type themeWarnings struct {
	mu   sync.Mutex
	last string
}

func (t *themeWarnings) warnOnce(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == name {
		return
	}
	t.last = name
	logx.Warnf("server.theme %q is not an installed or built-in theme; the shipped look is used", name)
}

func (s *Server) serveThemeCSS(w http.ResponseWriter, r *http.Request) {
	e := s.activeTheme()
	s.serveThemeFile(w, r, e, e.Path, "theme")
}

func (s *Server) serveThemeLogo(w http.ResponseWriter, r *http.Request) {
	e := s.activeTheme()
	s.serveThemeFile(w, r, e, e.Logo, "themelogo")
}

func (s *Server) serveThemeFavicon(w http.ResponseWriter, r *http.Request) {
	e := s.activeTheme()
	s.serveThemeFile(w, r, e, e.Favicon, "themefavicon")
}

func (s *Server) serveThemeFile(w http.ResponseWriter, r *http.Request, e themeEntry, path, kind string) {
	if !e.Builtin {
		s.serveConfiguredFile(w, r, path, kind)
		return
	}
	if path == "" {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, webFS.FS, path)
}

type themeCluster struct {
	Names  []string
	Active string
}

func (s *Server) themeCluster() themeCluster {
	entries := s.listThemes()
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return themeCluster{Names: names, Active: s.activeTheme().Name}
}

func (s *Server) settingsThemePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("theme"))
	if name != "" && !slices.ContainsFunc(s.listThemes(), func(e themeEntry) bool { return e.Name == name }) {
		flashStatus(w, http.StatusBadRequest, "No theme named "+name+".")
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		c.Server.Theme = name
		return nil
	}); err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: theme set to %q", name)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}
