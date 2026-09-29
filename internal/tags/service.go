// Package tags owns the tag catalog and every image's membership in it.
package tags

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/searchkw"
)

var (
	ErrInvalidTagName       = errors.New("invalid tag name")
	ErrTagNotFound          = errors.New("tag not found")
	ErrTagImplied           = errors.New("this tag is implied by another tag on the image; remove the parent, or add it yourself first to take ownership of the row")
	ErrCategoryNotFound     = errors.New("category not found")
	ErrBuiltinCategory      = errors.New("cannot delete built-in category")
	ErrBuiltinCategoryName  = errors.New("cannot rename a built-in category")
	ErrReservedCategoryName = errors.New("this name is used by a search filter (e.g. " + reservedCategoryHint() + ")")
	ErrNonCanonicalRating   = errors.New("rating category accepts only general, sensitive, questionable, explicit")
	ErrRatingTagImmutable   = errors.New("rating category tags cannot be renamed, moved, or turned into aliases")
	ErrRatingCategoryClosed = errors.New("nothing new can go into the rating category")
	ErrInvalidMoveTarget    = errors.New("tags must move to another existing category")
	ErrAliasNote            = errors.New("an alias carries no note; edit the tag it resolves to")

	// A matching colour goes into style attributes as trusted CSS.
	categoryColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

	ErrInvalidCategoryColor = errors.New("invalid category color (must be #rgb or #rrggbb)")

	// Names land in form-field names and in search syntax, where ':'
	// separates category from tag.
	categoryNameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

	ErrInvalidCategoryName = errors.New("invalid category name (use lowercase letters, digits, underscore, or hyphen)")

	ErrCategoryExists = errors.New("a category with this name already exists")
)

func IsValidCategoryColor(s string) bool { return categoryColorRe.MatchString(s) }

// Theme --cat-<rrggbb> variables, the seed colours and <input type=color>
// all need lowercase six-digit hex.
func normalizeCategoryColor(s string) string {
	if !IsValidCategoryColor(s) {
		return s
	}
	s = strings.ToLower(s)
	if len(s) == 4 {
		return string([]byte{'#', s[1], s[1], s[2], s[2], s[3], s[3]})
	}
	return s
}

func SafeCategoryColor(s string) string {
	if IsValidCategoryColor(s) {
		return s
	}
	return "#888888"
}

var (
	// Search keywords and "system" (the search-bar cheat sheet) already
	// own the "name:" prefix.
	reservedCategoryList = append(append([]string{}, searchkw.Keywords...), "system")

	// RatingLevels runs low to high; highest-wins resolution and the
	// rating ceiling rely on the order.
	RatingLevels = []string{"general", "sensitive", "questionable", "explicit"}
)

func IsCanonicalRating(name string) bool { return RatingRank(name) >= 0 }

func isReservedCategoryName(name string) bool { return slices.Contains(reservedCategoryList, name) }

func reservedCategoryHint() string {
	parts := make([]string, len(reservedCategoryList))
	for i, n := range reservedCategoryList {
		parts[i] = n + ":"
	}
	return strings.Join(parts, ", ")
}

type TagFilter struct {
	CategoryID *int64
	Prefix     string
	Sort       string // "name" | "usage" | "created" | "last_used"
	Order      string // "asc" or "desc"; empty keeps the sort's default
	PageIndex  int
	Limit      int
	Origin     string
	Type       string
	// UsedBy matches any source that applied the tag, not only the one
	// that created it.
	UsedBy        string
	CreatedAfter  string
	ConflictsOnly bool
	ShowZero      bool
	ZeroOnly      bool
	Stale         string
	FoldedOnly    bool
}

type Service struct {
	db          *db.DB
	ratingCatID int64
}

func New(database *db.DB) *Service {
	s := &Service{db: database}
	if err := database.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = 'rating'`,
	).Scan(&s.ratingCatID); err != nil {
		logx.Warnf("tags.New: rating category lookup failed: %v", err)
	}
	return s
}

func (s *Service) RatingCategoryID() int64 { return s.ratingCatID }

func (s *Service) inWriteTx(work func(*sql.Tx) error) error { return db.InWriteTx(s.db.Write, work) }

// RecalcDB leaves zero-usage tags in place so their aliases and
// implications survive an empty library.
func RecalcDB(database *db.DB) {
	if _, err := RecalcDBCount(database); err != nil {
		logx.Warnf("RecalcDB: %v", err)
	}
}

// A correlated-subquery UPDATE over every tag dominates sync time, so
// this zeroes the unused ones and fills the rest from one GROUP BY per
// tag_id chunk, freeing the writer between chunks.
func RecalcDBCount(database *db.DB) (int64, error) {
	const chunkSize = 2000

	var updated int64
	var maxID int64
	if err := database.Read.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM tags`).Scan(&maxID); err != nil {
		return 0, fmt.Errorf("max tag id: %w", err)
	}

	for start := int64(0); start <= maxID; start += chunkSize {
		end := start + chunkSize
		res, err := database.Write.Exec(`
			UPDATE tags SET usage_count = 0
			WHERE usage_count != 0
			  AND id >= ? AND id < ?
			  AND NOT EXISTS (
			      SELECT 1 FROM image_tags it
			      JOIN images i ON i.id = it.image_id
			      WHERE it.tag_id = tags.id AND i.is_missing = 0
			  )
		`, start, end)
		if err != nil {
			logx.Warnf("RecalcDBCount zero-out chunk [%d, %d): %v", start, end, err)
			return updated, fmt.Errorf("zero-out chunk [%d, %d): %w", start, end, err)
		}
		n, _ := res.RowsAffected()
		updated += n

		res, err = database.Write.Exec(`
			UPDATE tags SET usage_count = c.cnt
			FROM (
			    SELECT it.tag_id, COUNT(*) AS cnt FROM image_tags it
			    JOIN images i ON i.id = it.image_id
			    WHERE i.is_missing = 0 AND it.tag_id >= ? AND it.tag_id < ?
			    GROUP BY it.tag_id
			) c
			WHERE c.tag_id = tags.id AND tags.usage_count != c.cnt
		`, start, end)
		if err != nil {
			logx.Warnf("RecalcDBCount fill chunk [%d, %d): %v", start, end, err)
			return updated, fmt.Errorf("fill chunk [%d, %d): %w", start, end, err)
		}
		n, _ = res.RowsAffected()
		updated += n
	}
	return updated, nil
}

func (s *Service) RecalcCount() (int64, error) { return RecalcDBCount(s.db) }

// args already carries extraArgs, so deleteFn must append extraSQL to its
// own statement. The caller must RecalcIDs the returned tags.
func (s *Service) ChunkedDeleteWithTagRecalc(
	ctx context.Context,
	ids []int64,
	extraSQL string,
	extraArgs []any,
	deleteFn func(tx *sql.Tx, chunk []int64, placeholders string, args []any) error,
	afterCommit func(chunk []int64),
) (affected []int64, processed int, cancelled bool, err error) {
	const chunkSize = 500
	seen := map[int64]struct{}{}
	for start := 0; start < len(ids); start += chunkSize {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		chunk := ids[start:min(start+chunkSize, len(ids))]
		placeholders, chunkArgs := db.InPlaceholders(chunk)
		args := append(chunkArgs, extraArgs...)

		tx, err := s.db.Write.Begin()
		if err != nil {
			return tagIDsFromSet(seen), processed, false, err
		}
		// A read error aborts the chunk: a short set would leave stale
		// usage counts.
		touched, err := db.QueryIDs(tx,
			`SELECT DISTINCT tag_id FROM image_tags WHERE image_id IN (`+placeholders+`)`+extraSQL,
			args...)
		if err != nil {
			_ = tx.Rollback()
			return tagIDsFromSet(seen), processed, false, err
		}
		for _, tid := range touched {
			seen[tid] = struct{}{}
		}
		if err := deleteFn(tx, chunk, placeholders, args); err != nil {
			_ = tx.Rollback()
			return tagIDsFromSet(seen), processed, false, err
		}
		if err := tx.Commit(); err != nil {
			return tagIDsFromSet(seen), processed, false, err
		}
		if afterCommit != nil {
			afterCommit(chunk)
		}
		processed += len(chunk)
	}
	return tagIDsFromSet(seen), processed, cancelled, nil
}

func tagIDsFromSet(seen map[int64]struct{}) []int64 {
	if len(seen) == 0 {
		return nil
	}
	return slices.Collect(maps.Keys(seen))
}

func (s *Service) RecalcIDs(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return db.Chunked(ids, 500, func(chunk []int64) error {
		placeholders, args := db.InPlaceholders(chunk)
		if _, err := s.db.Write.Exec(`UPDATE tags SET usage_count = (
			SELECT COUNT(*) FROM image_tags it
			JOIN images i ON i.id = it.image_id
			WHERE it.tag_id = tags.id AND i.is_missing = 0
		) WHERE id IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("recalc usage_count chunk: %w", err)
		}
		return nil
	})
}
