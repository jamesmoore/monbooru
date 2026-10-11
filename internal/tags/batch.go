package tags

import (
	"context"
	"database/sql"

	"github.com/monbooru/monbooru/internal/db"
)

// BatchResult sums a batch over its chunks: Replaced counts the images
// whose rating an add replaced, Kept the rows a remove left because
// another tag on the image implies them.
type BatchResult struct {
	Processed int
	Changed   int
	Replaced  int
	Kept      int
	Cancelled bool
}

// BatchAddTags takes 100 images per transaction instead of 500 when a tag
// implies others: the per-row fan-out of a full chunk holds the writer for
// tens of seconds. Usage moves row by row, so no recount follows.
func (s *Service) BatchAddTags(ctx context.Context, imageIDs, tagIDs []int64, progress func(done, total int)) (BatchResult, error) {
	size := 500
	if s.anyImplies(tagIDs) {
		size = 100
	}
	return s.batchChunks(ctx, imageIDs, size, progress, func(tx *sql.Tx, chunk []int64) (BatchResult, error) {
		added, replaced, err := s.BatchAddTagsTx(tx, chunk, tagIDs)
		return BatchResult{Changed: added, Replaced: replaced}, err
	})
}

func (s *Service) BatchRemoveTags(ctx context.Context, imageIDs, tagIDs []int64, progress func(done, total int)) (BatchResult, error) {
	return s.batchChunks(ctx, imageIDs, 500, progress, func(tx *sql.Tx, chunk []int64) (BatchResult, error) {
		removed, kept, err := batchRemoveTagsTx(tx, chunk, tagIDs)
		return BatchResult{Changed: removed, Kept: kept}, err
	})
}

func (s *Service) batchChunks(ctx context.Context, imageIDs []int64, size int, progress func(done, total int),
	apply func(tx *sql.Tx, chunk []int64) (BatchResult, error),
) (BatchResult, error) {
	var res BatchResult
	for start := 0; start < len(imageIDs); start += size {
		if ctx.Err() != nil {
			res.Cancelled = true
			break
		}
		end := min(start+size, len(imageIDs))
		got, err := writeTx(s, func(tx *sql.Tx) (BatchResult, error) { return apply(tx, imageIDs[start:end]) })
		if err != nil {
			return res, err
		}
		res.Processed = end
		res.Changed += got.Changed
		res.Replaced += got.Replaced
		res.Kept += got.Kept
		if progress != nil {
			progress(end, len(imageIDs))
		}
	}
	return res, nil
}

func (s *Service) anyImplies(tagIDs []int64) bool {
	if len(tagIDs) == 0 {
		return false
	}
	placeholders, args := db.InPlaceholders(tagIDs)
	var n int
	if err := s.db.Read.QueryRow(
		`SELECT 1 FROM tag_implications WHERE parent_tag_id IN (`+placeholders+`) LIMIT 1`, args...,
	).Scan(&n); err != nil {
		return false
	}
	return n == 1
}
