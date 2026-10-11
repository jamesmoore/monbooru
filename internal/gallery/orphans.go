package gallery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
)

// PruneOrphanThumbnails fails the sweep on a failed id read: a partial set
// would make live thumbnails look orphaned.
func PruneOrphanThumbnails(ctx context.Context, database *db.DB, thumbnailsPath string, progress func(done, total int)) (removed, processed, total int, err error) {
	entries, err := os.ReadDir(thumbnailsPath)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read thumbnails dir: %w", err)
	}
	total = len(entries)
	ids, err := db.QueryIDsContext(ctx, database.Read, `SELECT id FROM images`)
	if err != nil {
		return 0, 0, total, err
	}
	known := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		known[id] = struct{}{}
	}
	report := func(done int) {
		if progress != nil {
			progress(done, total)
		}
	}
	report(0)
	for i, e := range entries {
		if ctx.Err() != nil {
			return removed, processed, total, nil
		}
		if (i+1)%50 == 0 {
			report(i + 1)
		}
		if e.IsDir() {
			continue
		}
		processed++
		id, ok := thumbnailOwner(e.Name())
		if !ok {
			continue
		}
		if _, held := known[id]; held {
			continue
		}
		if err := os.Remove(filepath.Join(thumbnailsPath, e.Name())); err == nil {
			removed++
		}
	}
	report(total)
	return removed, processed, total, nil
}

func thumbnailOwner(name string) (int64, bool) {
	for _, suffix := range []string{"_hover.webp", "_view.jpg", ".jpg"} {
		if idStr, ok := strings.CutSuffix(name, suffix); ok {
			id, err := strconv.ParseInt(idStr, 10, 64)
			return id, err == nil
		}
	}
	return 0, false
}
