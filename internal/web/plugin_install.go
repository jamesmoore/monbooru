package web

import (
	"path/filepath"

	"github.com/monbooru/monbooru/internal/plugins"
)

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

func (s *Server) effectivePlugins() []plugins.Effective {
	return plugins.EffectiveList(s.plugins(), s.pluginsDir())
}

func (s *Server) effective(name string) (plugins.Effective, bool) {
	return plugins.FindEffective(s.effectivePlugins(), name)
}
