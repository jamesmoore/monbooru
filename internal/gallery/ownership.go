package gallery

import (
	"os"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/logx"
)

// Files another user wrote (an rsync, rootless Podman's UID mapping)
// otherwise hit EACCES on a later rename or delete. A file behind a linked
// folder keeps its owner: a chown is the one thing a scan cannot undo.
func claimOwnership(galleryPath, path string) {
	if !storedInside(galleryPath, path) {
		logx.Debugf("chown %q: outside the gallery folder, left as it is", path)
		return
	}
	if err := os.Chown(path, os.Getuid(), os.Getgid()); err != nil && !os.IsNotExist(err) {
		logx.Debugf("chown %q: %v", path, err)
	}
}

func storedInside(root, path string) bool {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	pathReal, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return PathInside(rootReal, pathReal)
}
