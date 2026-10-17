package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/registry"
	"github.com/monbooru/monbooru/internal/shared/logx"
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

const builtinLightDir = registry.LightThemeDir

// themeVars are the custom properties a theme sets: the stable surface
// the themes registry builds on, beside one --cat-<hex> per built-in
// category colour. Rules and every other variable may change in any
// release. The nav hues have no default in main.css.
var themeVars = []string{
	"--bg", "--bg-surface", "--bg-input", "--border", "--fg", "--fg-dim",
	"--accent", "--link", "--success", "--warning", "--error",
	"--monloader", "--tagger", "--plugin", "--fav", "--remove", "--alternate",
	"--scrim", "--media-bg", "--font-mono",
	"--nav-images", "--nav-inbox", "--nav-categories", "--nav-tags",
	"--nav-collections", "--nav-relations", "--nav-settings",
}

var builtinThemes = []themeEntry{
	{Name: "dark", Builtin: true},
	{
		Name: "light", Builtin: true,
		Path:    builtinLightDir + "/theme.css",
		Logo:    builtinLightDir + "/logo.png",
		Favicon: builtinLightDir + "/favicon.png",
	},
}

func (s *Server) themesDir() string { return s.configSubdir("themes") }

// themeSeedFile records the bytes each file was seeded with: a file still
// holding them is an untouched copy, which a release may refresh. One the
// operator edited, or one seeded before the record existed and since
// moved, is left alone.
const themeSeedFile = ".seed"

// A deleted copy is not rebuilt.
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
	seeded := readThemeSeed(light)
	changed := false
	for _, name := range []string{"theme.css", "logo.png", "favicon.png"} {
		body, err := registry.FS.ReadFile(builtinLightDir + "/" + name)
		if err != nil {
			continue
		}
		want := sha256Hex(body)
		path := filepath.Join(light, name)
		if cur, err := os.ReadFile(path); err == nil {
			switch have := sha256Hex(cur); {
			case have == want:
				if seeded[name] != want {
					seeded[name], changed = want, true
				}
				continue
			case have != seeded[name] && (seeded[name] == "" || seeded[name] == want):
				logx.Infof("themes: %s was edited and is kept", path)
				continue
			case have != seeded[name]:
				logx.Infof("themes: %s is not the copy monbooru seeded; the bundled one moved and is left out", path)
				continue
			}
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			logx.Warnf("themes: could not write the example theme: %v", err)
			continue
		}
		seeded[name], changed = want, true
	}
	if changed {
		var b strings.Builder
		for _, name := range slices.Sorted(maps.Keys(seeded)) {
			fmt.Fprintf(&b, "%s  %s\n", seeded[name], name)
		}
		if err := os.WriteFile(filepath.Join(light, themeSeedFile), []byte(b.String()), 0o644); err != nil {
			logx.Warnf("themes: could not record the seeded theme: %v", err)
		}
	}
}

func readThemeSeed(dir string) map[string]string {
	seeded := map[string]string{}
	data, err := os.ReadFile(filepath.Join(dir, themeSeedFile))
	if err != nil {
		return seeded
	}
	for line := range strings.Lines(string(data)) {
		if sum, name, ok := strings.Cut(strings.TrimSpace(line), "  "); ok {
			seeded[name] = sum
		}
	}
	return seeded
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
		folder := it.IsDir()
		if !folder && it.Type()&os.ModeSymlink != 0 {
			fi, err := os.Stat(filepath.Join(dir, it.Name()))
			folder = err == nil && fi.IsDir()
		}
		if folder {
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
	if s.themeName() == "" {
		return builtinThemes[0]
	}
	return s.activeThemeIn(s.listThemes())
}

func (s *Server) activeThemeIn(entries []themeEntry) themeEntry {
	name := s.themeName()
	if name == "" {
		return builtinThemes[0]
	}
	for _, e := range entries {
		if e.Name == name {
			return e
		}
	}
	s.themeWarn.warnOnce(name)
	return builtinThemes[0]
}

func (s *Server) themeName() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return strings.TrimSpace(s.cfg.Server.Theme)
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
	if path == "" {
		http.NotFound(w, r)
		return
	}
	if !e.Builtin {
		// An archive gives every theme it unpacks one mtime.
		setGalleryScopedCache(w, "custom", kind+"-"+url.PathEscape(e.Name), path)
		http.ServeFile(w, r, path)
		return
	}
	http.ServeFileFS(w, r, registry.FS, path)
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
	return themeCluster{Names: names, Active: s.activeThemeIn(entries).Name}
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
