package gallery

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type EmptyDir struct {
	Path    string
	ModTime time.Time
}

// ScanEmptyDirs counts a dot directory as content: .stfolder and .git
// matter exactly when they look empty. ReadDir reports a linked directory
// as a non-directory, so a link is content too and is never followed.
func ScanEmptyDirs(b *Boundary) ([]EmptyDir, error) {
	galleryPath := b.Root()
	var out []EmptyDir
	var walk func(dir string, depth int) (bool, error)
	walk = func(dir string, depth int) (bool, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if depth == 0 {
				return false, err
			}
			return false, nil
		}
		empty := true
		for _, e := range entries {
			sub := filepath.Join(dir, e.Name())
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || b.skips(sub, sub) {
				empty = false
				continue
			}
			childEmpty, err := walk(sub, depth+1)
			if err != nil {
				return false, err
			}
			empty = empty && childEmpty
		}
		if !empty || depth == 0 {
			return empty, nil
		}
		rel, err := filepath.Rel(galleryPath, dir)
		if err != nil {
			return empty, nil
		}
		var mod time.Time
		if info, statErr := os.Stat(dir); statErr == nil {
			mod = info.ModTime()
		}
		out = append(out, EmptyDir{Path: filepath.ToSlash(rel), ModTime: mod})
		return empty, nil
	}
	if _, err := walk(galleryPath, 0); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b EmptyDir) int { return cmp.Compare(a.Path, b.Path) })
	return out, nil
}

// RemoveEmptyDirs leaves a folder that gained content since the scan:
// os.Remove will not empty a directory.
func RemoveEmptyDirs(bound *Boundary, paths []string) int {
	galleryPath := bound.Root()
	deepestFirst := slices.Clone(paths)
	slices.SortFunc(deepestFirst, func(a, b string) int {
		return strings.Count(b, "/") - strings.Count(a, "/")
	})
	removed := 0
	for _, p := range deepestFirst {
		dir, err := bound.ResolveSubdir(p)
		if err != nil || filepath.Clean(dir) == filepath.Clean(galleryPath) {
			continue
		}
		// os.Remove would unlink a file just as happily.
		if info, statErr := os.Lstat(dir); statErr != nil || !info.IsDir() {
			continue
		}
		// A folder already gone is not counted: the handler reports
		// len(paths) - removed as no longer empty.
		if err := os.Remove(dir); err != nil {
			continue
		}
		removed++
	}
	return removed
}
