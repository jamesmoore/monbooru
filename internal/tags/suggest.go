package tags

import (
	"database/sql"
	"fmt"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// A tag on more images than this adds noise, and pulling in its carriers
// makes the candidate GROUP BY slow on a large library.
const relatedMaxTagUsage = 10000

// Candidates are ranked from image_tags alone under an inner LIMIT;
// images is joined last, so the is_missing filter only sees that buffer.
func (s *Service) RelatedImages(imageID int64, limit int, ratingCeiling string) ([]models.Image, error) {
	excluded := s.RatingTagIDsAbove(ratingCeiling)

	var sourceFileType string
	if err := s.db.Read.QueryRow(`SELECT file_type FROM images WHERE id = ?`, imageID).Scan(&sourceFileType); err != nil {
		return nil, err
	}
	typePredicate := "i.file_type = 'cbz'"
	if sourceFileType != "cbz" {
		typePredicate = "i.file_type != 'cbz'"
	}

	candidatesExtra := ""
	args := []any{imageID, relatedMaxTagUsage, relatedGeneralTagsCap, imageID}
	if len(excluded) > 0 {
		placeholders, excludedArgs := db.InPlaceholders(excluded)
		candidatesExtra = ` AND NOT EXISTS (
		         SELECT 1 FROM image_tags x
		         WHERE x.image_id = theirs.image_id
		           AND x.tag_id IN (` + placeholders + `)
		     )`
		args = append(args, excludedArgs...)
	}
	args = append(args, limit*2+5, limit)

	rows, err := s.db.Read.Query(
		`WITH my_tags AS (
		     SELECT tag_id FROM (
		         SELECT it.tag_id, tc.name AS cat_name,
		                ROW_NUMBER() OVER (PARTITION BY tc.name
		                                   ORDER BY t.usage_count ASC, t.id ASC) AS rn
		         FROM image_tags it
		         JOIN tags t ON t.id = it.tag_id
		         JOIN tag_categories tc ON tc.id = t.category_id
		         WHERE it.image_id = ? AND tc.name != 'meta'
		           AND t.usage_count <= ?
		     )
		     WHERE cat_name != 'general' OR rn <= ?
		 ),
		 candidates AS (
		     SELECT theirs.image_id, COUNT(*) AS shared
		     FROM image_tags theirs
		     WHERE theirs.tag_id IN (SELECT tag_id FROM my_tags)
		       AND theirs.image_id != ?`+candidatesExtra+`
		     GROUP BY theirs.image_id
		     ORDER BY shared DESC, theirs.image_id DESC
		     LIMIT ?
		 )
		 SELECT i.id, i.file_type, i.page_count
		 FROM candidates c
		 JOIN images i ON i.id = c.image_id
		 WHERE i.is_missing = 0 AND `+typePredicate+`
		 ORDER BY c.shared DESC, c.image_id DESC
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []models.Image
	for rows.Next() {
		var img models.Image
		var pageCount *int
		if err := rows.Scan(&img.ID, &img.FileType, &pageCount); err != nil {
			return nil, err
		}
		img.PageCount = pageCount
		out = append(out, img)
	}
	return out, rows.Err()
}

func (s *Service) SuggestTags(prefix string, limit int) ([]models.Tag, error) {
	return SuggestUsageRanked(s.db, prefix, "", false, limit)
}

func SuggestUsageRanked(database *db.DB, prefix, categoryName string, requireUsage bool, limit int) ([]models.Tag, error) {
	prefix = db.EscapeLike(NormalizeTagName(prefix))
	// Ranked on tags alone so idx_tags_active_usage can stop at the
	// limit; with the category join the planner drives from
	// tag_categories and temp-sorts every tag.
	baseSQL := `SELECT t.id, t.name, tc.name, tc.color, t.usage_count
	            FROM (SELECT id, name, category_id, usage_count
	                  FROM tags
	                  WHERE is_alias = 0
	                    %s
	                    AND name LIKE ? ESCAPE '\'
	                    %s
	                  ORDER BY usage_count DESC, name ASC
	                  LIMIT ?) t
	            JOIN tag_categories tc ON tc.id = t.category_id
	            ORDER BY t.usage_count DESC, t.name ASC`
	usageClause := ""
	if requireUsage {
		usageClause = "AND usage_count > 0"
	}
	catClause := ""
	var catArgs []any
	if categoryName != "" {
		catClause = "AND category_id = (SELECT id FROM tag_categories WHERE name = ?)"
		catArgs = []any{categoryName}
	}

	run := func(pat string, prior []models.Tag, remaining int, nameNotLike string) ([]models.Tag, error) {
		extra := catClause
		qargs := make([]any, 0, 2+len(catArgs))
		qargs = append(qargs, pat)
		qargs = append(qargs, catArgs...)
		if nameNotLike != "" {
			extra = extra + ` AND name NOT LIKE ? ESCAPE '\'`
			qargs = append(qargs, nameNotLike)
		}
		qargs = append(qargs, remaining)
		scanned, err := db.QueryAll(database.Read, ScanTag, fmt.Sprintf(baseSQL, usageClause, extra), qargs...)
		if err != nil {
			return prior, err
		}
		seen := map[int64]bool{}
		for _, t := range prior {
			seen[t.ID] = true
		}
		for _, t := range scanned {
			if seen[t.ID] {
				continue
			}
			prior = append(prior, t)
			seen[t.ID] = true
		}
		return prior, nil
	}

	out, err := run(prefix+"%", nil, limit, "")
	if err != nil {
		return nil, err
	}
	if len(out) < limit {
		out, err = run("%"+prefix+"%", out, limit-len(out), prefix+"%")
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func (s *Service) SuggestTagsInCategory(prefix, categoryName string, limit int) ([]models.Tag, error) {
	return db.QueryAll(s.db.Read, ScanTag,
		`SELECT t.id, t.name, tc.name, tc.color, t.usage_count
		 FROM (SELECT id, name, category_id, usage_count
		       FROM tags
		       WHERE category_id = (SELECT id FROM tag_categories WHERE name = ?)
		         AND name LIKE ? ESCAPE '\' AND is_alias = 0
		       ORDER BY usage_count DESC
		       LIMIT ?) t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 ORDER BY t.usage_count DESC`,
		categoryName, db.EscapeLike(NormalizeTagName(prefix))+"%", limit)
}

func ScanTag(rows *sql.Rows) (models.Tag, error) {
	var t models.Tag
	err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount)
	return t, err
}
