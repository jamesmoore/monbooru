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

// MoveImageResult's Moved and Renamed report what actually changed, not
// what the caller asked for; PrevDir is set only when the file left its
// folder.
type MoveImageResult struct {
	NewCanonicalPath string
	NewFolderPath    string
	PrevDir          string
	Moved            bool
	Renamed          bool
	Suffixed         bool
}

// PlaceImage leaves alone whichever of folder and name is nil. Callers
// holding a watcher must run it under a job type the watcher suppresses,
// or its events race the DB update.
func PlaceImage(database *db.DB, b *Boundary, id int64, targetFolder, newName *string) (*MoveImageResult, error) {
	return placeImage(database, b, id, targetFolder, newName, "place")
}

type placement struct {
	oldCanonical string
	destDir      string
	newFolder    string
	base         string
	moved        bool
	renamed      bool
}

// plan touches nothing on disk, so a dry run can share it with the run.
func plan(database *db.DB, b *Boundary, id int64, folder, name *string, verb string) (placement, error) {
	var p placement
	if name != nil {
		p.base = strings.TrimSpace(*name)
		if p.base == "" || p.base != filepath.Base(p.base) || p.base == "." || p.base == ".." {
			return p, fmt.Errorf("invalid file name %q", p.base)
		}
		p.base = TruncateFilename(p.base, maxNameBytes)
	}

	oldCanonical, oldFolder, err := loadMoveSource(database, b, id, verb)
	if err != nil {
		return p, err
	}
	p.oldCanonical = oldCanonical

	// The destination is already root-bounded by ResolveSubdir.
	p.destDir, p.newFolder = filepath.Dir(oldCanonical), oldFolder
	if folder != nil {
		dir, resolveErr := b.ResolveSubdir(*folder)
		if resolveErr != nil {
			return p, resolveErr
		}
		rel, relErr := filepath.Rel(b.Root(), dir)
		if relErr != nil {
			return p, fmt.Errorf("resolve folder: %w", relErr)
		}
		if rel == "." {
			rel = ""
		}
		p.destDir, p.newFolder = dir, filepath.ToSlash(rel)
	}

	if name != nil {
		if ext := filepath.Ext(oldCanonical); !strings.EqualFold(filepath.Ext(p.base), ext) {
			p.base += ext
		}
	} else {
		p.base = filepath.Base(oldCanonical)
	}

	if err := b.Check(filepath.Join(p.destDir, p.base)); err != nil {
		return p, err
	}
	p.moved, p.renamed = p.newFolder != oldFolder, p.base != filepath.Base(oldCanonical)
	return p, nil
}

// PlannedPath writes nothing, so the caller adds each result to claimed for
// the rows after it; numbered reports that the destination was taken.
func PlannedPath(database *db.DB, b *Boundary, id int64, folder, name *string, claimed map[string]struct{}) (path string, numbered bool, err error) {
	p, err := plan(database, b, id, folder, name, "place")
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

func placeImage(database *db.DB, b *Boundary, id int64, folder, name *string, verb string) (*MoveImageResult, error) {
	p, err := plan(database, b, id, folder, name, verb)
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

// An empty dropOldPath demotes the current canonical to an alias instead
// of dropping it, as a reactivation wants.
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

func loadMoveSource(database *db.DB, b *Boundary, id int64, verb string) (oldCanonical, oldFolder string, err error) {
	galleryPath := b.Root()
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
	if err := b.Check(oldCanonical); err != nil {
		return "", "", fmt.Errorf("refusing to %s image %d: %w", verb, id, err)
	}
	return oldCanonical, oldFolder, nil
}

// The unique helpers check only the disk: a stale alias row holding the path
// would otherwise fail the UNIQUE constraint mid-tx with no useful error.
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

// The file moves before the tx opens: across filesystems the move is a full
// copy, which would hold the only write connection throughout.
func commitRename(database *db.DB, verb string, id int64, oldCanonical, newPath string, newFolder *string) error {
	if err := moveIntoPlace(oldCanonical, newPath); err != nil {
		return fmt.Errorf("rename file: %w", err)
	}
	err := func() error {
		tx, err := database.Write.Begin()
		if err != nil {
			return fmt.Errorf("begin %s tx: %w", verb, err)
		}
		defer func() { _ = tx.Rollback() }()
		update, args := `UPDATE images SET canonical_path = ? WHERE id = ?`, []any{newPath, id}
		if newFolder != nil {
			update = `UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`
			args = []any{newPath, *newFolder, id}
		}
		if _, err := tx.Exec(update, args...); err != nil {
			return fmt.Errorf("update images row: %w", err)
		}
		if _, err := tx.Exec(
			`UPDATE image_paths SET path = ? WHERE image_id = ? AND is_canonical = 1`,
			newPath, id,
		); err != nil {
			return fmt.Errorf("update image_paths row: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s tx: %w", verb, err)
		}
		return nil
	}()
	if err != nil {
		if rnErr := moveIntoPlace(newPath, oldCanonical); rnErr != nil {
			logx.Errorf("%s: reverse rename for %d after a failed update: %v (original: %v)", verb, id, rnErr, err)
		}
		return err
	}
	return nil
}

func renameSuffix(stem, ext string, i int) string { return fmt.Sprintf("%s%02d%s", stem, i, ext) }

// renamed must say whether the name actually changes, not whether one was
// given.
func placementSuffix(renamed bool) func(stem, ext string, i int) string {
	if renamed {
		return renameSuffix
	}
	return uploadSuffix
}

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

func PromoteCanonicalByPath(database *db.DB, b *Boundary, imageID int64, newPath string) error {
	if err := b.Check(newPath); err != nil {
		return err
	}
	return promoteCanonical(database, b.Root(), imageID, newPath, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE image_paths SET is_canonical = 1 WHERE image_id = ? AND path = ?`, imageID, newPath)
		return err
	})
}

// PromoteCanonicalByPathID takes the path's row id; newPath must be that
// row's path.
func PromoteCanonicalByPathID(database *db.DB, b *Boundary, imageID, pathID int64, newPath string) error {
	if err := b.Check(newPath); err != nil {
		return err
	}
	return promoteCanonical(database, b.Root(), imageID, newPath, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE image_paths SET is_canonical = 1 WHERE id = ?`, pathID)
		return err
	})
}

// DeleteAliasPath only forgets the row; the file is the caller's to unlink.
func DeleteAliasPath(database *db.DB, pathID int64) error {
	_, err := database.Write.Exec(`DELETE FROM image_paths WHERE id = ?`, pathID)
	return err
}

func DeleteAliasPaths(database *db.DB, pathIDs []int64) error {
	placeholders, args := db.InPlaceholders(pathIDs)
	_, err := database.Write.Exec(`DELETE FROM image_paths WHERE id IN (`+placeholders+`)`, args...)
	return err
}
