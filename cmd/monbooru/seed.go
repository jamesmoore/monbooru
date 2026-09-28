package main

import (
	"os"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
)

// The images bake MONBOORU_CONTAINER and nothing else sets it.
func inContainer() bool {
	return os.Getenv("MONBOORU_CONTAINER") != ""
}

// Absolute, so a service unit's working directory cannot move the paths later.
func hostSeed(configPath string) func(*config.Config) {
	dir := filepath.Dir(configPath)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return func(cfg *config.Config) {
		cfg.Paths.DataPath = filepath.Join(dir, "data")
		cfg.Paths.ModelPath = filepath.Join(dir, "data", "models")
		gallery := filepath.Join(dir, "gallery")
		if err := os.MkdirAll(gallery, 0o755); err != nil {
			logx.Warnf("could not create a gallery folder at %s: %v", gallery, err)
		}
		cfg.Galleries[0].GalleryPath = gallery
	}
}
