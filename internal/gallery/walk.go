package gallery

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// WalkTree walks root the way filepath.WalkDir does, except that a symlink
// is reported as whatever it points at and one leading to a directory is
// descended into. Entries beneath it keep the link's own path prefix, so
// folder_path, the containment checks and the watcher's registrations all
// work on the names the operator sees rather than on where the bytes live.
//
// A link whose target will not stat is skipped: it is neither a file nor a
// folder, and there is nothing under it to report. A directory link is
// skipped too when its target resolves inside root, when it contains root,
// or when an earlier link already led into it - the walk reaches those
// another way, two names for one directory would ingest its files twice,
// and a link to an ancestor would walk the whole gallery a second time
// under the link's own name.
func WalkTree(root string, fn fs.WalkDirFunc) error {
	return WalkTreeUnder(root, root, fn)
}

// WalkTreeUnder is WalkTree with the link tests taken against boundary
// rather than against root. A caller walking a directory that appeared
// inside the gallery needs the two to differ: the walk starts at the new
// directory, and the tree its links must not escape is still the gallery.
func WalkTreeUnder(root, boundary string, fn fs.WalkDirFunc) error {
	bound, err := filepath.EvalSymlinks(boundary)
	if err != nil {
		bound = filepath.Clean(boundary)
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		rootReal = filepath.Clean(root)
	}
	w := treeWalker{
		rootReal: bound,
		followed: map[string]struct{}{bound: {}, rootReal: {}},
		fn:       fn,
	}
	info, err := os.Stat(root)
	if err != nil {
		err = fn(root, nil, err)
	} else {
		err = w.walk(root, root, fs.FileInfoToDirEntry(info))
	}
	if errors.Is(err, fs.SkipAll) || errors.Is(err, fs.SkipDir) {
		return nil
	}
	return err
}

type treeWalker struct {
	rootReal string
	followed map[string]struct{}
	fn       fs.WalkDirFunc
}

// walk reports d, then the entries of dir beneath path. dir is where the
// bytes live and path is the name they are reported under; the two part
// company once the walk has stepped through a link.
func (w *treeWalker) walk(dir, path string, d fs.DirEntry) error {
	if err := w.fn(path, d, nil); err != nil {
		if errors.Is(err, fs.SkipDir) {
			return nil
		}
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// SkipDir off the error-report call means this directory, which
		// is already as far as the walk gets; returning it verbatim would
		// unwind to WalkTree and truncate the whole walk silently.
		if err := w.fn(path, d, err); err != nil && !errors.Is(err, fs.SkipDir) {
			return err
		}
		return nil
	}
	for _, e := range entries {
		sub, subPath := filepath.Join(dir, e.Name()), filepath.Join(path, e.Name())
		descend, entry, ok := w.follow(sub, e)
		if !ok {
			continue
		}
		if descend != "" {
			if err := w.walk(descend, subPath, entry); err != nil {
				return err
			}
			continue
		}
		if err := w.fn(subPath, entry, nil); err != nil {
			// SkipDir on a file means the rest of this directory.
			if errors.Is(err, fs.SkipDir) {
				return nil
			}
			return err
		}
	}
	return nil
}

// follow answers what one entry is: the directory to descend into (empty
// when there is none), the entry to report it under, and whether the walk
// covers it at all. A link is answered as its target and recorded, so a
// second link to the same directory is left alone rather than walked twice.
func (w *treeWalker) follow(p string, e fs.DirEntry) (dir string, entry fs.DirEntry, ok bool) {
	if e.Type()&fs.ModeSymlink == 0 {
		if e.IsDir() {
			return p, e, true
		}
		return "", e, true
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", nil, false
	}
	e = fs.FileInfoToDirEntry(info)
	if !e.IsDir() {
		return "", e, true
	}
	target, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", nil, false
	}
	if _, seen := w.followed[target]; seen || PathInside(w.rootReal, target) || PathInside(target, w.rootReal) {
		return "", nil, false
	}
	w.followed[target] = struct{}{}
	return target, e, true
}
