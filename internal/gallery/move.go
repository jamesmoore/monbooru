package gallery

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
)

// MoveImageResult reports the new location of a moved image so the caller
// can render it and invalidate caches without re-querying. Moved and
// Renamed say which halves actually changed, which is not the same as
// which halves the caller filled; Suffixed says the destination was taken
// and the file had to be numbered aside. PrevDir is the directory the file
// left, set only when it left one, so a batch can tell what it emptied.
type MoveImageResult struct {
	NewCanonicalPath string
	NewFolderPath    string
	PrevDir          string
	Moved            bool
	Renamed          bool
	Suffixed         bool
}

// PlaceImage moves image id into targetFolder and renames its file in one
// step, so a file being filed never passes through a third path that
// neither half describes. A nil folder or name leaves that half of the
// path alone. Callers that hold a watcher should gate this under a job
// type the watcher suppresses, otherwise the resulting CREATE/REMOVE
// events race with the DB update.
func PlaceImage(database *db.DB, galleryPath string, id int64, targetFolder, newName *string) (*MoveImageResult, error) {
	return placeImage(database, galleryPath, id, targetFolder, newName, "place")
}

// placement is where a file would end up, before anything on disk is
// touched: the resolved directory, the basename it would take, and which
// halves that actually changes.
type placement struct {
	oldCanonical string
	destDir      string
	newFolder    string
	base         string
	moved        bool
	renamed      bool
}

// plan resolves the destination without creating or moving anything, so
// the same rules answer both the run and a dry run of it.
func plan(database *db.DB, galleryPath string, id int64, folder, name *string, verb string) (placement, error) {
	var p placement
	if name != nil {
		p.base = strings.TrimSpace(*name)
		if p.base == "" || p.base != filepath.Base(p.base) || p.base == "." || p.base == ".." {
			return p, fmt.Errorf("invalid file name %q", p.base)
		}
		// The naming templates bound every segment they render at
		// maxNameBytes; a name typed into the dialog reaches the same
		// filesystem and gets the same bound.
		p.base = TruncateFilename(p.base, maxNameBytes)
	}

	oldCanonical, oldFolder, err := loadMoveSource(database, galleryPath, id, verb)
	if err != nil {
		return p, err
	}
	p.oldCanonical = oldCanonical

	// The destination is already root-bounded by ResolveSubdir.
	p.destDir, p.newFolder = filepath.Dir(oldCanonical), oldFolder
	if folder != nil {
		dir, resolveErr := ResolveSubdir(galleryPath, *folder)
		if resolveErr != nil {
			return p, resolveErr
		}
		rel, relErr := filepath.Rel(galleryPath, dir)
		if relErr != nil {
			return p, fmt.Errorf("resolve folder: %w", relErr)
		}
		if rel == "." {
			rel = ""
		}
		// folder_path is stored "/"-separated on every platform.
		p.destDir, p.newFolder = dir, filepath.ToSlash(rel)
	}

	if name != nil {
		if ext := filepath.Ext(oldCanonical); !strings.EqualFold(filepath.Ext(p.base), ext) {
			p.base += ext
		}
	} else {
		p.base = filepath.Base(oldCanonical)
	}

	p.moved, p.renamed = p.newFolder != oldFolder, p.base != filepath.Base(oldCanonical)
	return p, nil
}

// PlannedPath answers where PlaceImage would file id, numbering the
// destination aside exactly as the run would when something already holds it:
// a file on disk, or a path claimed by an earlier row of the same scope.
// numbered says that happened. Nothing is created, moved or written.
func PlannedPath(database *db.DB, galleryPath string, id int64, folder, name *string, claimed map[string]struct{}) (path string, numbered bool, err error) {
	p, err := plan(database, galleryPath, id, folder, name, "place")
	if err != nil {
		return "", false, err
	}
	path = filepath.Join(p.destDir, p.base)
	if !p.moved && !p.renamed {
		return path, false, nil
	}
	resolved := uniquePathIn(p.destDir, p.base, claimed, placementSuffix(p.renamed))
	return resolved, resolved != path, nil
}

func placeImage(database *db.DB, galleryPath string, id int64, folder, name *string, verb string) (*MoveImageResult, error) {
	p, err := plan(database, galleryPath, id, folder, name, verb)
	if err != nil {
		return nil, err
	}
	oldCanonical, destDir, newFolder, base := p.oldCanonical, p.destDir, p.newFolder, p.base
	moved, renamed := p.moved, p.renamed
	if !moved && !renamed {
		return &MoveImageResult{
			NewCanonicalPath: oldCanonical,
			NewFolderPath:    newFolder,
		}, nil
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("create destination folder: %w", err)
	}

	newPath := uniquePathBy(destDir, base, placementSuffix(renamed))

	// The unique helpers only check the filesystem, not image_paths. A
	// stale alias row for a different image (file long gone but row never
	// pruned) would otherwise trip the UNIQUE constraint on path mid-tx
	// with no useful diagnostic. Surface the collision up front so the
	// caller can suggest "prune duplicate paths" from the Settings
	// maintenance page.
	if err := refuseAliasCollision(database, id, newPath); err != nil {
		return nil, err
	}
	folderArg := &newFolder
	if folder == nil {
		folderArg = nil
	}
	if err := commitRename(database, verb, id, oldCanonical, newPath, folderArg); err != nil {
		return nil, err
	}

	res := &MoveImageResult{
		NewCanonicalPath: newPath,
		NewFolderPath:    newFolder,
		Moved:            moved,
		Renamed:          renamed,
		Suffixed:         newPath != filepath.Join(destDir, base),
	}
	if moved {
		res.PrevDir = filepath.Dir(oldCanonical)
	}
	return res, nil
}

// repointCanonical moves image id's canonical path to newPath: the row is
// pointed at it, the retired path leaves image_paths, and newPath becomes
// the canonical row (upserting, so an alias already holding it is promoted
// in place). An empty dropOldPath demotes whatever is canonical now to an
// alias instead of dropping it, which is what a reactivation wants - the
// path the row used to live at keeps its place in the history.
//
// e is the pool or a transaction, so a caller that must not half-apply the
// move can run it inside one.
func repointCanonical(e db.Execer, id int64, newPath, newFolder, dropOldPath string) error {
	if _, err := e.Exec(
		`UPDATE images SET canonical_path = ?, folder_path = ?, is_missing = 0 WHERE id = ?`,
		newPath, newFolder, id,
	); err != nil {
		return fmt.Errorf("point row at the new path: %w", err)
	}
	if dropOldPath == "" {
		if _, err := e.Exec(
			`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ? AND is_canonical = 1`, id,
		); err != nil {
			return fmt.Errorf("demote previous canonical: %w", err)
		}
	} else if _, err := e.Exec(
		`DELETE FROM image_paths WHERE image_id = ? AND path = ?`, id, dropOldPath,
	); err != nil {
		return fmt.Errorf("drop old canonical: %w", err)
	}
	if _, err := e.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1)
		 ON CONFLICT(path) DO UPDATE SET is_canonical = 1`, id, newPath,
	); err != nil {
		return fmt.Errorf("install new canonical: %w", err)
	}
	return nil
}

// loadMoveSource reads the row a move or rename acts on and refuses what
// neither can handle: a file already gone from disk, and a canonical_path
// that drifted outside the gallery root (mirroring DeleteImage). verb names
// the refused action in the error.
func loadMoveSource(database *db.DB, galleryPath string, id int64, verb string) (oldCanonical, oldFolder string, err error) {
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, id,
	).Scan(&oldCanonical, &oldFolder, &isMissing); err != nil {
		return "", "", fmt.Errorf("image %d not found: %w", id, err)
	}
	if isMissing == 1 {
		return "", "", fmt.Errorf("image %d is missing from disk", id)
	}
	if galleryPath != "" && !PathInside(galleryPath, oldCanonical) {
		return "", "", fmt.Errorf("refusing to %s %q outside gallery root %q", verb, oldCanonical, galleryPath)
	}
	return oldCanonical, oldFolder, nil
}

// refuseAliasCollision rejects a destination another image already records
// as an alias: a stale image_paths row would otherwise trip the UNIQUE
// constraint mid-tx with no useful diagnostic.
func refuseAliasCollision(database *db.DB, id int64, newPath string) error {
	var collidingImage int64
	switch err := database.Read.QueryRow(
		`SELECT image_id FROM image_paths WHERE path = ? AND image_id != ?`,
		newPath, id,
	).Scan(&collidingImage); {
	case err == nil:
		return fmt.Errorf("destination collides with an existing alias on image %d", collidingImage)
	case errors.Is(err, sql.ErrNoRows):
		return nil
	default:
		return fmt.Errorf("check destination for an alias collision: %w", err)
	}
}

// commitRename repoints both path rows and moves the file inside the open
// tx, so a failed move rolls the row updates back automatically. The
// watcher suppresses events while the job runs, so the window where newPath
// exists on disk before the commit does not race a concurrent ingest. A
// commit failure (rare - SQLite COMMIT is essentially an fsync) moves the
// file back; if that fails too the library is wedged and needs a manual
// sync. The move goes through moveIntoPlace rather than os.Rename: a
// symlinked folder can put two folders of one gallery on different
// filesystems, which the kernel refuses to rename across.
// newFolder nil leaves folder_path alone, which is what a rename in place
// wants.
func commitRename(database *db.DB, verb string, id int64, oldCanonical, newPath string, newFolder *string) error {
	tx, err := database.Write.Begin()
	if err != nil {
		return fmt.Errorf("begin %s tx: %w", verb, err)
	}
	update, args := `UPDATE images SET canonical_path = ? WHERE id = ?`, []any{newPath, id}
	if newFolder != nil {
		update = `UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`
		args = []any{newPath, *newFolder, id}
	}
	if _, err := tx.Exec(update, args...); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("update images row: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET path = ? WHERE image_id = ? AND is_canonical = 1`,
		newPath, id,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("update image_paths row: %w", err)
	}
	if err := moveIntoPlace(oldCanonical, newPath); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("rename file: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if rnErr := moveIntoPlace(newPath, oldCanonical); rnErr != nil {
			logx.Errorf("%s: reverse rename for %d after commit fail: %v (original: %v)", verb, id, rnErr, err)
		}
		return fmt.Errorf("commit %s tx: %w", verb, err)
	}
	return nil
}

// renameSuffix appends a zero-padded counter to the stem (name01.png,
// name02.png, ...) so rename collisions read like the batch rename's numbered
// sequence instead of UniqueDestPath's `_N` upload suffixes.
func renameSuffix(stem, ext string, i int) string { return fmt.Sprintf("%s%02d%s", stem, i, ext) }

// placementSuffix is how a taken destination is numbered aside. The style
// follows what actually happens, not what the caller filled in: a name that
// renders to the one the file already has is not a rename, so its collision
// numbers the way a move's does.
func placementSuffix(renamed bool) func(stem, ext string, i int) string {
	if renamed {
		return renameSuffix
	}
	return uploadSuffix
}

// promoteCanonical demotes every path of the image, promotes the one
// promote selects, and repoints the row at newPath. folder_path travels
// with it: promoting a path in another folder moves the image for
// folder: / folderonly: search and for the cached folder tree.
func promoteCanonical(database *db.DB, galleryPath string, imageID int64, newPath string, promote func(*sql.Tx) error) error {
	newFolder := FolderPath(galleryPath, newPath)
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ?`, imageID); err != nil {
			return err
		}
		if err := promote(tx); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`,
			newPath, newFolder, imageID)
		return err
	})
}

// PromoteCanonicalByPath makes the named alias path of an image its
// canonical one.
func PromoteCanonicalByPath(database *db.DB, galleryPath string, imageID int64, newPath string) error {
	return promoteCanonical(database, galleryPath, imageID, newPath, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE image_paths SET is_canonical = 1 WHERE image_id = ? AND path = ?`, imageID, newPath)
		return err
	})
}

// PromoteCanonicalByPathID is PromoteCanonicalByPath keyed by the
// image_paths row instead of its path, for the callers that already
// resolved it.
func PromoteCanonicalByPathID(database *db.DB, galleryPath string, imageID, pathID int64, newPath string) error {
	return promoteCanonical(database, galleryPath, imageID, newPath, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE image_paths SET is_canonical = 1 WHERE id = ?`, pathID)
		return err
	})
}

// DeleteAliasPath forgets one non-canonical path row. The file it named is
// the caller's to unlink.
func DeleteAliasPath(database *db.DB, pathID int64) error {
	_, err := database.Write.Exec(`DELETE FROM image_paths WHERE id = ?`, pathID)
	return err
}

// DeleteAliasPaths is DeleteAliasPath over a chunk of rows, for the
// duplicate-removal job that walks them in batches.
func DeleteAliasPaths(database *db.DB, pathIDs []int64) error {
	placeholders, args := db.InPlaceholders(pathIDs)
	_, err := database.Write.Exec(`DELETE FROM image_paths WHERE id IN (`+placeholders+`)`, args...)
	return err
}
