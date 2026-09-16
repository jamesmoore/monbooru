package gallery

import (
	"os"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/logx"
)

// claimOwnership chowns path to the current process UID/GID so later
// rename/delete operations don't hit EACCES on files originally written
// by a different user (rsynced from another machine, rootless Podman
// where the container's UID 0 doesn't match the bind mount's owner).
//
// A file the walk reached through a symlinked folder is left alone. It
// lives in a tree the operator keeps elsewhere and shares with whatever
// else reads it, and rewriting its owner is the one thing here that a
// scan cannot undo and nobody asked for. The resolve costs a handful of
// lstats at a call site that has just read the whole file to hash it.
//
// Best-effort: failures log at debug and never abort the caller. ENOENT
// is silenced because callers race deletions and watcher events.
func claimOwnership(galleryPath, path string) {
	if !storedInside(galleryPath, path) {
		logx.Debugf("chown %q: outside the gallery folder, left as it is", path)
		return
	}
	if err := os.Chown(path, os.Getuid(), os.Getgid()); err != nil && !os.IsNotExist(err) {
		logx.Debugf("chown %q: %v", path, err)
	}
}

// storedInside reports whether path's bytes physically sit under root,
// resolving every link on both sides. NamedInside answers the name, which
// is the right question for the serve gate; this one asks where the file
// actually is, and an unanswerable question counts as outside.
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
