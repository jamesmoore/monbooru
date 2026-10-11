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
	Removed   int
	Cancelled bool
}

func (spec StripSpec) predicate() (string, []any, error) {
	switch spec.Mode {
	case "user":
		// An implied row carries no tagger either, and is not the user's own.
		return ` AND is_auto = 0 AND is_implied = 0 AND (tagger_name IS NULL OR tagger_name = '')`, nil, nil
	case "auto":
		if spec.Name != "" {
			return ` AND is_auto = 1 AND is_implied = 0 AND tagger_name = ?`, []any{spec.Name}, nil
		}
		return ` AND is_auto = 1 AND is_implied = 0`, nil, nil
	case "source":
		return ` AND is_auto = 0 AND tagger_name = ?`, []any{spec.Name}, nil
	case "source-all":
		return ` AND is_auto = 0 AND tagger_name <> '' AND tagger_name IS NOT NULL`, nil, nil
	case "stale":
		return ` AND stale = 1`, nil, nil
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
		removed, err := stripChunkTx(tx, chunk, pred, extra)
		return BatchResult{Changed: removed}, err
	})
	return StripResult{Processed: res.Processed, Removed: res.Changed, Cancelled: res.Cancelled}, err
}

func stripChunkTx(tx *sql.Tx, chunk []int64, pred string, extra []any) (int, error) {
	in, args := db.InPlaceholders(chunk)
	args = append(args, extra...)
	if _, err := tx.Exec(`UPDATE tags SET usage_count = MAX(0, usage_count - d.n)
		FROM (SELECT tag_id, COUNT(*) AS n FROM image_tags
		      WHERE image_id IN (`+in+`)`+pred+`
		        AND EXISTS (SELECT 1 FROM images i WHERE i.id = image_tags.image_id AND i.is_missing = 0)
		      GROUP BY tag_id) d
		WHERE tags.id = d.tag_id`, args...); err != nil {
		return 0, fmt.Errorf("drop usage: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM image_tags WHERE image_id IN (`+in+`)`+pred, args...)
	if err != nil {
		return 0, fmt.Errorf("strip image_tags: %w", err)
	}
	removed, _ := res.RowsAffected()
	orphans, err := pruneOrphanedImpliedTx(tx, chunk)
	if err != nil {
		return 0, fmt.Errorf("sweep implied: %w", err)
	}
	return int(removed) + orphans, nil
}
