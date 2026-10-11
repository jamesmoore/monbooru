package gallery

import (
	"database/sql"
	"fmt"

	"github.com/monbooru/monbooru/internal/db"
)

// RankCeiling's zero value hides nothing.
type RankCeiling struct {
	Rank   int
	Active bool
}

func (c RankCeiling) and(imagesAlias string) (string, []any) {
	if !c.Active {
		return "", nil
	}
	return ` AND ` + imagesAlias + `.rating_rank <= ?`, []any{c.Rank}
}

func scalarCountUnder(database *db.DB, baseWhere string, ceiling RankCeiling) (int, error) {
	where, args := ceiling.and("i")
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images i WHERE `+baseWhere+where, args...,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func InboxCountUnder(database *db.DB, ceiling RankCeiling) (int, error) {
	return scalarCountUnder(database, "i.is_missing = 0 AND i.is_inbox = 1", ceiling)
}

func PhashMissingUnder(database *db.DB, ceiling RankCeiling) (int, error) {
	return scalarCountUnder(database, "i.phash IS NULL AND i.is_missing = 0", ceiling)
}

// SourceLabelCountsUnderQuery counts secondary origins too, since the
// source: filter matches any of an image's sources.
func SourceLabelCountsUnderQuery(database *db.DB, limit int, ceiling RankCeiling) ([]SourceLabelCount, error) {
	under, args := ceiling.and("i")
	args = append(args, limit)
	return db.QueryAll(database.Read, db.ScanRow(func(c *SourceLabelCount) []any {
		return []any{&c.Source, &c.Count}
	}),
		`SELECT s.site, COUNT(DISTINCT s.image_id) c FROM image_sources s
		 WHERE s.site != '' AND EXISTS (SELECT 1 FROM images i WHERE i.id = s.image_id AND i.is_missing = 0`+under+`)
		 GROUP BY s.site ORDER BY c DESC, s.site ASC LIMIT ?`,
		args...)
}

func FolderTreeUnder(database *db.DB, ceiling RankCeiling) ([]FolderNode, error) {
	where, args := ceiling.and("i")
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
