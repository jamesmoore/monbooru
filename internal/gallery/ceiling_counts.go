package gallery

import (
	"database/sql"
	"fmt"

	"github.com/monbooru/monbooru/internal/db"
)

func excludeNotExists(imageCol string, excludeIDs []int64) (string, []any) {
	if len(excludeIDs) == 0 {
		return "", nil
	}
	placeholders, args := db.InPlaceholders(excludeIDs)
	return ` AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = ` + imageCol + ` AND it.tag_id IN (` + placeholders + `))`, args
}

func scalarCountUnder(database *db.DB, baseWhere string, excludeIDs []int64) (int, error) {
	where, args := excludeNotExists("i.id", excludeIDs)
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images i WHERE `+baseWhere+where, args...,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func InboxCountUnder(database *db.DB, excludeIDs []int64) (int, error) {
	return scalarCountUnder(database, "i.is_missing = 0 AND i.is_inbox = 1", excludeIDs)
}

func PhashMissingUnder(database *db.DB, excludeIDs []int64) (int, error) {
	return scalarCountUnder(database, "i.phash IS NULL AND i.is_missing = 0", excludeIDs)
}

// SourceLabelCountsUnderQuery counts secondary origins too, since the
// source: filter matches any of an image's sources.
func SourceLabelCountsUnderQuery(database *db.DB, limit int, excludeIDs []int64) ([]SourceLabelCount, error) {
	if limit <= 0 {
		limit = 25
	}
	exclude, args := excludeNotExists("s.image_id", excludeIDs)
	args = append(args, limit)
	return db.QueryAll(database.Read, func(rows *sql.Rows) (SourceLabelCount, error) {
		var c SourceLabelCount
		err := rows.Scan(&c.Source, &c.Count)
		return c, err
	},
		`SELECT s.site, COUNT(DISTINCT s.image_id) c FROM image_sources s
		 WHERE s.site != '' AND EXISTS (SELECT 1 FROM images i WHERE i.id = s.image_id AND i.is_missing = 0)`+exclude+`
		 GROUP BY s.site ORDER BY c DESC, s.site ASC LIMIT ?`,
		args...)
}

func FolderTreeUnder(database *db.DB, excludeIDs []int64) ([]FolderNode, error) {
	if len(excludeIDs) == 0 {
		return FolderTree(database)
	}
	where, args := excludeNotExists("i.id", excludeIDs)
	flat, err := db.QueryAll(database.Read, scanFolderRow,
		`SELECT COALESCE(i.folder_path, ''), COUNT(*) FROM images i
		 WHERE i.is_missing = 0`+where+`
		 GROUP BY i.folder_path ORDER BY i.folder_path`,
		args...)
	if err != nil {
		return nil, err
	}
	return buildFolderTree(flat), nil
}

type folderCount struct {
	path  string
	count int
}

func scanFolderRow(rows *sql.Rows) (folderCount, error) {
	var fc folderCount
	if err := rows.Scan(&fc.path, &fc.count); err != nil {
		return fc, fmt.Errorf("scanning folder row: %w", err)
	}
	return fc, nil
}
