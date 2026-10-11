package library

import (
	"fmt"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
)

type Layout struct {
	Galleries   []config.Gallery
	DataPath    string
	ModelPath   string
	ThemesDir   string
	PluginsDir  string
	Ignore      []string
	DerivePaths func(name string) (dbPath, thumbnailsPath string)
}

func (l Layout) galleryOnFolder(folder, skip string) string {
	for _, g := range l.Galleries {
		if g.Name != skip && gallery.SameFolder(g.GalleryPath, folder) {
			return g.Name
		}
	}
	return ""
}

type ownFolder struct{ path, what string }

func (l Layout) monbooruFolders() []ownFolder {
	return []ownFolder{
		{l.ModelPath, "monbooru's model folder"},
		{l.ThemesDir, "monbooru's theme folder"},
		{l.PluginsDir, "monbooru's plugin folder"},
	}
}

func holds(outer, inner string) bool {
	return outer != "" && inner != "" && (gallery.SameFolder(outer, inner) || gallery.Encloses(outer, inner))
}

// inOwnFolder refuses a gallery folder inside one monbooru writes to: its
// files would go with that folder's cleanup or rename. names are galleries
// not configured yet, whose data folders count too.
func (l Layout) inOwnFolder(galleryPath string, names ...string) error {
	folders := make([]ownFolder, 0, len(l.Galleries)+len(names))
	for _, g := range l.Galleries {
		folders = append(folders, ownFolder{filepath.Dir(g.DBPath), "the data folder of gallery " + g.Name})
	}
	for _, name := range names {
		db, _ := l.DerivePaths(name)
		folders = append(folders, ownFolder{filepath.Dir(db), "the data folder of gallery " + name})
	}
	if gallery.SameFolder(galleryPath, l.DataPath) {
		return fmt.Errorf("%s is monbooru's data folder; pick a folder outside it", galleryPath)
	}
	for _, f := range append(folders, l.monbooruFolders()...) {
		if holds(f.path, galleryPath) {
			return fmt.Errorf("%s is inside %s; pick a folder outside it", galleryPath, f.what)
		}
	}
	return nil
}

func (l Layout) dataFolderHolds(name string) (dir, what string) {
	db, _ := l.DerivePaths(name)
	dir = filepath.Dir(db)
	held := l.monbooruFolders()
	for _, g := range l.Galleries {
		held = append(held, ownFolder{g.GalleryPath, "the folder of gallery " + g.Name})
	}
	for _, h := range held {
		if holds(dir, h.path) {
			return dir, h.what
		}
	}
	return dir, ""
}
