package gallery

import (
	"database/sql"
	"fmt"
	"os"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
)

type DeleteImageResult struct {
	CanonicalPath string
	FolderPath    string
	IsMissing     bool
}

// DeleteImage runs onImageDelete before the row delete:
// dup_groups.original_image_id has no cascade and would block it.
func DeleteImage(database *db.DB, b *Boundary, thumbnailsPath string, id int64, removeAllTags func(*sql.Tx, int64) error, onImageDelete func(*sql.Tx, int64) error) (*DeleteImageResult, error) {
	var canonPath, folderPath, fileType string
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing, file_type FROM images WHERE id = ?`, id,
	).Scan(&canonPath, &folderPath, &isMissing, &fileType); err != nil {
		return nil, fmt.Errorf("image not found: %w", err)
	}
	aliases, err := AliasPathsFor(database.Read, []int64{id})
	if err != nil {
		return nil, fmt.Errorf("alias paths for image %d: %w", id, err)
	}

	tx, err := database.Write.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin delete image %d: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := removeAllTags(tx, id); err != nil {
		return nil, fmt.Errorf("remove tags for image %d: %w", id, err)
	}
	if onImageDelete != nil {
		if err := onImageDelete(tx, id); err != nil {
			return nil, fmt.Errorf("relations cleanup for image %d: %w", id, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM images WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("delete image row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit delete image %d: %w", id, err)
	}

	RemoveImageArtifacts(thumbnailsPath, id, fileType)

	result := &DeleteImageResult{
		CanonicalPath: canonPath,
		FolderPath:    folderPath,
		IsMissing:     isMissing == 1,
	}

	if !result.IsMissing {
		UnlinkImageFile(b, canonPath, id)
	}
	UnlinkAliasFiles(b, id, aliases[id])

	return result, nil
}

// AliasCopy's Size is the row's, which every true copy shares.
type AliasCopy struct {
	Path string
	Size int64
}

// AliasPathsFor must run before the rows go: image_paths cascades with images.
func AliasPathsFor(q db.Querier, ids []int64) (map[int64][]AliasCopy, error) {
	placeholders, args := db.InPlaceholders(ids)
	if placeholders == "" {
		return nil, nil
	}
	rows, err := q.Query(
		`SELECT ip.image_id, ip.path, i.file_size
		   FROM image_paths ip
		   JOIN images i ON i.id = ip.image_id
		  WHERE ip.is_canonical = 0 AND ip.image_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64][]AliasCopy{}
	for rows.Next() {
		var id int64
		var c AliasCopy
		if err := rows.Scan(&id, &c.Path, &c.Size); err != nil {
			return nil, err
		}
		out[id] = append(out[id], c)
	}
	return out, rows.Err()
}

// UnlinkAliasFiles takes the copies with the row, or the next sync ingests
// one as a new untagged image. A copy no longer the row's length stays: with
// the watcher off it can be another file written over a recorded path.
func UnlinkAliasFiles(b *Boundary, id int64, copies []AliasCopy) {
	for _, c := range copies {
		if info, err := os.Stat(c.Path); err == nil && info.Size() != c.Size {
			logx.Warnf("delete image %d: %q holds %d bytes, not the row's %d - left alone",
				id, c.Path, info.Size(), c.Size)
			continue
		}
		UnlinkImageFile(b, c.Path, id)
	}
}

func RemoveImageArtifacts(thumbnailsPath string, id int64, fileType string) {
	_ = os.Remove(ThumbnailPath(thumbnailsPath, id))
	_ = os.Remove(hoverPath(thumbnailsPath, id))
	_ = os.Remove(viewRenditionPath(thumbnailsPath, id))
	if fileType == "" || fileType == "cbz" {
		removeMangaCache(thumbnailsPath, id)
	}
}

// UnlinkImageFile does not trust canonPath: a hand-edited DB or a
// repointed mount can point it outside the root.
func UnlinkImageFile(b *Boundary, canonPath string, id int64) {
	if canonPath == "" {
		return
	}
	if !PathInside(b.Root(), canonPath) {
		logx.Warnf("delete image %d: refusing to unlink %q outside gallery root %q", id, canonPath, b.Root())
		return
	}
	if err := b.Check(canonPath); err != nil {
		logx.Warnf("delete image %d: leaving %q alone: %v", id, canonPath, err)
		return
	}
	if err := os.Remove(canonPath); err != nil && !os.IsNotExist(err) {
		logx.Warnf("delete image file %q: %v", canonPath, err)
	}
}
