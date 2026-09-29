package gallery

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// WalkTree reports a link as its target, under the link's own name, and walks
// no file under two names: it skips a directory link into the root, above it,
// into a walked or fenced folder, and a real folder an earlier link walked.
func WalkTree(b *Boundary, fn fs.WalkDirFunc) error {
	return WalkTreeUnder(b.Root(), b, fn)
}

func WalkTreeUnder(root string, b *Boundary, fn fs.WalkDirFunc) error {
	bound, err := filepath.EvalSymlinks(b.Root())
	if err != nil {
		bound = filepath.Clean(b.Root())
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		rootReal = filepath.Clean(root)
	}
	w := treeWalker{
		rootReal: bound,
		followed: map[string]struct{}{bound: {}, rootReal: {}},
		fn:       fn,
		bound:    b,
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
	bound    *Boundary
}

// dir is where the bytes live and path the name they are reported under;
// they differ past a link.
func (w *treeWalker) walk(dir, path string, d fs.DirEntry) error {
	if err := w.fn(path, d, nil); err != nil {
		if errors.Is(err, fs.SkipDir) {
			return nil
		}
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// SkipDir here means this directory; returned as is, it would end
		// the whole walk silently.
		if err := w.fn(path, d, err); err != nil && !errors.Is(err, fs.SkipDir) {
			return err
		}
		return nil
	}
	for _, e := range entries {
		sub, subPath := filepath.Join(dir, e.Name()), filepath.Join(path, e.Name())
		if w.bound.skips(subPath, sub) {
			continue
		}
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

func (w *treeWalker) follow(p string, e fs.DirEntry) (dir string, entry fs.DirEntry, ok bool) {
	if e.Type()&fs.ModeSymlink == 0 {
		if e.IsDir() {
			if _, linked := w.followed[p]; linked {
				return "", nil, false
			}
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
	if w.walkedBy(target) || PathInside(target, w.rootReal) || w.bound.leadsOut(target) {
		return "", nil, false
	}
	w.followed[target] = struct{}{}
	return target, e, true
}

// followed holds the root, so a target inside the root is walked by it.
func (w *treeWalker) walkedBy(target string) bool {
	for f := range w.followed {
		if PathInside(f, target) {
			return true
		}
	}
	return false
}
