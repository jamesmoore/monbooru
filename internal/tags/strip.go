package tags

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/monbooru/monbooru/internal/db"
)

// StripSpec picks the rows a strip removes: Name narrows auto to one
// tagger and names the source for source.
type StripSpec struct {
	Mode string
	Name string
}

type StripResult struct {
	Processed int
	Touched   int
	Removed   int
	Kept      int
	Cancelled bool
}

// The row selectors the strip and the per-image removers share. An implied
// row carries no tagger either, and is not the user's own.
const (
	userRowsPred   = ` AND is_auto = 0 AND is_implied = 0 AND (tagger_name IS NULL OR tagger_name = '')`
	autoRowsPred   = ` AND is_auto = 1 AND is_implied = 0`
	sourceRowsPred = ` AND is_auto = 0`
	staleRowsPred  = ` AND stale = 1`
)

func (spec StripSpec) predicate() (string, []any, error) {
	switch spec.Mode {
	case "user":
		return userRowsPred, nil, nil
	case "auto":
		if spec.Name != "" {
			return autoRowsPred + ` AND tagger_name = ?`, []any{spec.Name}, nil
		}
		return autoRowsPred, nil, nil
	case "source":
		return sourceRowsPred + ` AND tagger_name = ?`, []any{spec.Name}, nil
	case "source-all":
		return sourceRowsPred + ` AND tagger_name <> '' AND tagger_name IS NOT NULL`, nil, nil
	case "stale":
		return staleRowsPred, nil, nil
	case "all":
		return ``, nil, nil
	}
	return "", nil, fmt.Errorf("unknown strip mode %q", spec.Mode)
}

// StripTags takes 500 images per transaction and keeps usage_count in step
// as it goes, so a run that stops early leaves the counts right for what
// it took.
func (s *Service) StripTags(ctx context.Context, imageIDs []int64, spec StripSpec, progress func(done, total int)) (StripResult, error) {
	pred, extra, err := spec.predicate()
	if err != nil {
		return StripResult{}, err
	}
	res, err := s.batchChunks(ctx, imageIDs, 500, progress, func(tx *sql.Tx, chunk []int64) (BatchResult, error) {
		removed, touched, demoted, err := stripChunkTx(tx, chunk, pred, extra)
		return BatchResult{Touched: touched, Changed: removed, Kept: demoted}, err
	})
	return StripResult{Processed: res.Processed, Touched: res.Touched, Removed: res.Changed, Kept: res.Kept, Cancelled: res.Cancelled}, err
}

// An owned row a parent on the image still implies is demoted, as the
// per-image removers do; all takes every row, so it demotes none.
func stripChunkTx(tx *sql.Tx, chunk []int64, pred string, extra []any) (removed, touched, demoted int, err error) {
	in, args := db.InPlaceholders(chunk)
	args = append(args, extra...)
	var demotedRows [][2]int64
	if pred != "" {
		demotedRows, err = db.QueryAll(tx, db.ScanRow(func(r *[2]int64) []any { return []any{&r[0], &r[1]} }),
			`UPDATE image_tags SET is_implied = 1, confidence = NULL, tagger_name = NULL, stale = 0
			 WHERE image_id IN (`+in+`)`+pred+` AND is_implied = 0 AND `+parentOnImage+`
			 RETURNING image_id, tag_id`, args...)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("demote implied: %w", err)
		}
		for _, r := range demotedRows {
			if _, err := tx.Exec(`DELETE FROM image_tag_sources WHERE image_id = ? AND tag_id = ?`, r[0], r[1]); err != nil {
				return 0, 0, 0, fmt.Errorf("drop tag sources: %w", err)
			}
		}
	}
	if err := tx.QueryRow(`SELECT COUNT(DISTINCT image_id) FROM image_tags WHERE image_id IN (`+in+`)`+pred, args...).Scan(&touched); err != nil {
		return 0, 0, 0, fmt.Errorf("count images: %w", err)
	}
	if _, err := tx.Exec(`UPDATE tags SET usage_count = MAX(0, usage_count - d.n)
		FROM (SELECT tag_id, COUNT(*) AS n FROM image_tags
		      WHERE image_id IN (`+in+`)`+pred+`
		        AND EXISTS (SELECT 1 FROM images i WHERE i.id = image_tags.image_id AND i.is_missing = 0)
		      GROUP BY tag_id) d
		WHERE tags.id = d.tag_id`, args...); err != nil {
		return 0, 0, 0, fmt.Errorf("drop usage: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM image_tags WHERE image_id IN (`+in+`)`+pred, args...)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("strip image_tags: %w", err)
	}
	deleted, _ := res.RowsAffected()
	orphans, err := pruneOrphanedImpliedTx(tx, chunk)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("sweep implied: %w", err)
	}
	// A demoted row whose parent the strip also took went with the sweep.
	for _, r := range demotedRows {
		var kept bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM image_tags WHERE image_id = ? AND tag_id = ?)`, r[0], r[1]).Scan(&kept); err != nil {
			return 0, 0, 0, err
		}
		if kept {
			demoted++
		}
	}
	return int(deleted) + orphans, touched, demoted, nil
}
