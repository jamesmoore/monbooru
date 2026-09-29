package web

import (
	"os"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/plugins"
)

// The launch line stays off PluginConfig and out of monbooru.toml, so a
// web-writable exec line cannot exist.
type effectivePlugin struct {
	config.PluginConfig
	Launch    plugins.Launch
	Installed bool
}

func (s *Server) pluginsDir() string { return s.configSubdir("plugins") }

// Absolute: a plugin runs with its folder as cwd, where a relative path
// would resolve against the folder.
func (s *Server) configSubdir(name string) string {
	if s.configPath == "" {
		return ""
	}
	dir := filepath.Join(filepath.Dir(s.configPath), name)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// Seeds nothing: an example here would be executable code.
func (s *Server) ensurePluginsDir() {
	dir := s.pluginsDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logx.Warnf("plugins: could not create %s: %v", dir, err)
	}
}

func reservePluginName(name string) (string, bool) {
	if name == monloaderApp {
		return "the name belongs to the companion", true
	}
	if err := config.ValidatePluginName(name); err != nil {
		return err.Error(), true
	}
	return "", false
}

func (s *Server) effectivePlugins() []effectivePlugin {
	blocks := s.plugins()
	out := make([]effectivePlugin, 0, len(blocks))
	byName := make(map[string]int, len(blocks))
	for _, p := range blocks {
		byName[p.Name] = len(out)
		out = append(out, effectivePlugin{PluginConfig: p})
	}
	for _, ins := range plugins.Discover(s.pluginsDir(), reservePluginName) {
		i, ok := byName[ins.Name]
		if !ok {
			out = append(out, effectivePlugin{
				PluginConfig: config.PluginConfig{Name: ins.Name},
				Launch:       ins,
				Installed:    true,
			})
			continue
		}
		out[i].Launch, out[i].Installed = ins, true
	}
	return out
}

func (s *Server) effective(name string) (effectivePlugin, bool) {
	for _, p := range s.effectivePlugins() {
		if p.Name == name {
			return p, true
		}
	}
	return effectivePlugin{}, false
}
