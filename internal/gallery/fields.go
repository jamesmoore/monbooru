package gallery

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
)

type ImageFlag string

const (
	FlagFavorited ImageFlag = "is_favorited"
	FlagInbox     ImageFlag = "is_inbox"
)

// ToggleFlag reports the new value; sql.ErrNoRows for an unknown id.
func ToggleFlag(database *db.DB, id int64, flag ImageFlag) (bool, error) {
	col := string(flag)
	var v int
	err := database.Write.QueryRow(`UPDATE images SET `+col+` = 1 - `+col+` WHERE id = ? RETURNING `+col, id).Scan(&v)
	return v == 1, err
}

func ToggleFlags(database *db.DB, ids []int64, flag ImageFlag) error {
	col := string(flag)
	return db.Chunked(ids, 500, func(chunk []int64) error {
		in, args := db.InPlaceholders(chunk)
		_, err := database.Write.Exec(`UPDATE images SET `+col+` = 1 - `+col+` WHERE id IN (`+in+`)`, args...)
		return err
	})
}

func SetFlag(database *db.DB, ids []int64, flag ImageFlag, on bool) error {
	return db.Chunked(ids, 500, func(chunk []int64) error {
		in, args := db.InPlaceholders(chunk)
		_, err := database.Write.Exec(`UPDATE images SET `+string(flag)+` = ? WHERE id IN (`+in+`)`, append([]any{on}, args...)...)
		return err
	})
}

// SetNote reports sql.ErrNoRows for an unknown id.
func SetNote(database *db.DB, id int64, note string) error {
	res, err := database.Write.Exec(`UPDATE images SET note = ? WHERE id = ?`, note, id)
	return requireAffected(res, err, sql.ErrNoRows)
}

// SetReadPage clears the position for page 0.
func SetReadPage(database *db.DB, id int64, page int) error {
	var value any
	if page > 0 {
		value = page
	}
	_, err := database.Write.Exec(`UPDATE images SET last_read_page = ? WHERE id = ?`, value, id)
	return err
}

// SetScheduledLookup resets the ladder on an opt-in: a spent one would
// leave nothing to try.
func SetScheduledLookup(database *db.DB, ids []int64, backends []string, on bool) error {
	sets := make([]string, len(backends))
	vals := make([]any, len(backends))
	for i, b := range backends {
		sets[i] = lookup.FlagColumn(b) + " = ?"
		vals[i] = on
	}
	now := time.Now()
	return db.Chunked(ids, 500, func(chunk []int64) error {
		in, args := db.InPlaceholders(chunk)
		return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`UPDATE images SET `+strings.Join(sets, ", ")+` WHERE id IN (`+in+`)`, append(append([]any{}, vals...), args...)...); err != nil {
				return err
			}
			if !on {
				return nil
			}
			return lookup.ResetFor(tx, in, args, backends, now)
		})
	})
}

// StampUploadBatch marks one upload's rows so the inbox shows them as one
// cluster.
func StampUploadBatch(database *db.DB, ids []int64, token int64) error {
	return db.Chunked(ids, 500, func(chunk []int64) error {
		in, args := db.InPlaceholders(chunk)
		_, err := database.Write.Exec(`UPDATE images SET upload_batch = ? WHERE id IN (`+in+`)`, append([]any{token}, args...)...)
		return err
	})
}

func SetDimensions(ctx context.Context, database *db.DB, id int64, width, height int) error {
	_, err := database.Write.ExecContext(ctx, `UPDATE images SET width = ?, height = ? WHERE id = ?`, width, height, id)
	return err
}

// ReadComicInfo is nil, nil for an archive that carries none.
func ReadComicInfo(path string) (*models.MangaMetadata, error) {
	m, err := OpenManga(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = m.Close() }()
	return metadata.ParseComicInfo(m.Reader())
}

// ApplyReExtract runs in one transaction, so a failure never leaves the
// new source_type over missing metadata. A nil durationSec leaves the
// column alone, as a false rewriteManga leaves manga_metadata; a nil
// manga with it set drops the row.
func ApplyReExtract(ctx context.Context, database *db.DB, imageID int64, sourceType string, durationSec *float64, sd *models.SDMetadata, comfy *models.ComfyUIMetadata, manga *models.MangaMetadata, rewriteManga bool) error {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE images SET source_type = ? WHERE id = ?`, sourceType, imageID); err != nil {
		return fmt.Errorf("update source_type: %w", err)
	}
	if durationSec != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE images SET duration_seconds = ? WHERE id = ?`, *durationSec, imageID); err != nil {
			return fmt.Errorf("update duration_seconds: %w", err)
		}
	}
	if err := replaceGenerationMetadata(ctx, tx, imageID, sd, comfy); err != nil {
		return err
	}
	if rewriteManga {
		if _, err := tx.ExecContext(ctx, `DELETE FROM manga_metadata WHERE image_id = ?`, imageID); err != nil {
			return fmt.Errorf("clear manga_metadata: %w", err)
		}
		if manga != nil {
			manga.ImageID = imageID
			if err := insertMangaMeta(tx, manga); err != nil {
				return fmt.Errorf("insert manga_metadata: %w", err)
			}
		}
	}
	return tx.Commit()
}

// RebuildComfyTerms re-derives every workflow's terms, not only the ones
// indexed under an older version.
func RebuildComfyTerms(ctx context.Context, database *db.DB, progress func(processed, total int, message string)) (int, error) {
	if _, err := database.Write.ExecContext(ctx, `UPDATE comfyui_metadata SET terms_version = 0`); err != nil {
		return 0, err
	}
	return BackfillComfyTerms(ctx, database, progress)
}

// RenameCollectionFindRelations carries the opt-in to the new label. On a
// merge the target's own flag wins; the DELETE is merge-only, since on a
// case-only rename it would match the recased row.
func RenameCollectionFindRelations(database *db.DB, oldName, newName string, merging bool) error {
	if _, err := database.Write.Exec(`UPDATE OR IGNORE collection_find_relations SET name = ? WHERE name = ?`, newName, oldName); err != nil {
		return err
	}
	if !merging {
		return nil
	}
	return SetCollectionFindRelations(database, oldName, false)
}
