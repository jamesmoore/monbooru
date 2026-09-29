package relations

import (
	"context"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
)

type PhashBackfillProgress func(processed, total int, message string)

// BackfillPhashes reads the ids up front so no read cursor stays open for
// the whole job.
func BackfillPhashes(ctx context.Context, database *db.DB, thumbnailsPath string, progress PhashBackfillProgress) (processed, updated int, err error) {
	ids, err := db.QueryIDs(database.Read,
		`SELECT id FROM images WHERE phash IS NULL AND is_missing = 0 ORDER BY id`)
	if err != nil {
		return 0, 0, err
	}

	return gallery.BackfillWalk(ctx, ids, progress, "phash", "", func(id int64) error {
		h, err := gallery.RecomputeAndStorePhash(ctx, database, id, thumbnailsPath)
		if err != nil {
			return err
		}
		PhashStored(database, id, h)
		return nil
	})
}
