package library

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
)

// respellRelativeRows makes absolute the paths stored while the root was
// relative, against the working directory the root was resolved in. It reads
// only rows outside the root's prefix: two range scans, empty once done.
func respellRelativeRows(database *db.DB, root string) error {
	if !filepath.IsAbs(root) {
		return nil
	}
	type pathRow struct {
		id, imageID int64
		path        string
		canonical   bool
	}
	sep := filepath.Separator
	rows, err := db.QueryAll(database.Write, db.ScanRow(func(r *pathRow) []any {
		return []any{&r.id, &r.imageID, &r.path, &r.canonical}
	}),
		`SELECT id, image_id, path, is_canonical FROM image_paths WHERE path < ?
		 UNION ALL
		 SELECT id, image_id, path, is_canonical FROM image_paths WHERE path >= ?`,
		root+string(sep), root+string(sep+1))
	if err != nil {
		return err
	}
	rows = slices.DeleteFunc(rows, func(r pathRow) bool { return filepath.IsAbs(r.path) })
	if len(rows) == 0 {
		return nil
	}
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		for _, r := range rows {
			abs, err := filepath.Abs(r.path)
			if err != nil {
				return err
			}
			var twinID int64
			var twinCanonical bool
			err = tx.QueryRow(`SELECT id, is_canonical FROM image_paths WHERE path = ?`, abs).Scan(&twinID, &twinCanonical)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return err
			case !r.canonical:
				if _, err := tx.Exec(`DELETE FROM image_paths WHERE id = ?`, r.id); err != nil {
					return err
				}
				continue
			case twinCanonical:
				logx.Warnf("image %d: %q is also the file of another image; left for the next sync", r.imageID, abs)
				continue
			default:
				if _, err := tx.Exec(`DELETE FROM image_paths WHERE id = ?`, twinID); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`UPDATE image_paths SET path = ? WHERE id = ?`, abs, r.id); err != nil {
				return err
			}
			if r.canonical {
				if _, err := tx.Exec(`UPDATE images SET canonical_path = ? WHERE id = ?`, abs, r.imageID); err != nil {
					return err
				}
			}
		}
		return refileRootRows(tx, root)
	})
}

// A relative root gave a file landed by absolute path no folder. Its scan
// runs only beside a respell, so once.
func refileRootRows(tx *sql.Tx, root string) error {
	type imageRow struct {
		id   int64
		path string
	}
	sep := filepath.Separator
	rows, err := db.QueryAll(tx, db.ScanRow(func(r *imageRow) []any { return []any{&r.id, &r.path} }),
		`SELECT id, canonical_path FROM images WHERE folder_path = '' AND canonical_path >= ? AND canonical_path < ?`,
		root+string(sep), root+string(sep+1))
	if err != nil {
		return err
	}
	for _, r := range rows {
		if folder := gallery.FolderPath(root, r.path); folder != "" {
			if _, err := tx.Exec(`UPDATE images SET folder_path = ? WHERE id = ?`, folder, r.id); err != nil {
				return err
			}
		}
	}
	return nil
}
