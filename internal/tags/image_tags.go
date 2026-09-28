package tags

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

func (s *Service) AddTagToImage(imageID, tagID int64, isAuto bool, confidence *float64) error {
	_, err := s.AddTagToImageReportingDup(imageID, tagID, isAuto, confidence, "")
	return err
}

func (s *Service) AddTagsToImageFromTagger(imageID int64, tagIDs []int64, isAuto bool, taggerName string) error {
	return s.AddTagsToImageFromTaggerConf(imageID, tagIDs, nil, isAuto, taggerName)
}

// AddTagsToImageFromTaggerConf pairs confs[i] with tagIDs[i]; a nil or
// missing entry stores NULL.
func (s *Service) AddTagsToImageFromTaggerConf(imageID int64, tagIDs []int64, confs []*float64, isAuto bool, taggerName string) error {
	if len(tagIDs) == 0 {
		return nil
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		_, err := addTagsTx(tx, imageID, tagIDs, confs, isAuto, taggerName, s.ratingCatID)
		return err
	})
}

func addTagsTx(tx *sql.Tx, imageID int64, tagIDs []int64, confs []*float64, isAuto bool, via string, ratingCatID int64) ([]AddResult, error) {
	results := make([]AddResult, 0, len(tagIDs))
	for i, tagID := range tagIDs {
		var c *float64
		if i < len(confs) {
			c = confs[i]
		}
		added, promoted, displaced, err := addOneTagTx(tx, imageID, tagID, isAuto, c, via, ratingCatID)
		if err != nil {
			return results, err
		}
		results = append(results, AddResult{Added: added, Promoted: promoted, DisplacedRatings: displaced})
	}
	return results, nil
}

func addOneTagTx(tx *sql.Tx, imageID, tagID int64, isAuto bool, confidence *float64, taggerName string, ratingCatID int64) (added, promoted bool, displaced []string, err error) {
	added, promoted, err = addTagToImageTxReportingDup(tx, imageID, tagID, isAuto, confidence, taggerName, ratingCatID)
	if err != nil {
		return false, false, nil, err
	}
	if added || promoted {
		if displaced, err = pruneRatingsAfterAddTx(tx, ratingCatID, imageID, tagID, isAuto); err != nil {
			return false, false, nil, err
		}
	}
	return added, promoted, displaced, nil
}

type AddResult struct {
	Added            bool
	Promoted         bool
	DisplacedRatings []string
}

func (s *Service) AddTagToImageReportingDup(imageID, tagID int64, isAuto bool, confidence *float64, taggerName string) (AddResult, error) {
	var res AddResult
	err := s.inWriteTx(func(tx *sql.Tx) error {
		out, err := addTagsTx(tx, imageID, []int64{tagID}, []*float64{confidence}, isAuto, taggerName, s.ratingCatID)
		if err != nil {
			return err
		}
		res = out[0]
		return nil
	})
	if err != nil {
		return AddResult{}, err
	}
	return res, nil
}

func addTagToImageTxReportingDup(tx *sql.Tx, imageID, tagID int64, isAuto bool, confidence *float64, taggerName string, ratingCatID int64) (bool, bool, error) {
	isAutoInt := 0
	if isAuto {
		isAutoInt = 1
	}
	// tagger_name is the source label on manual rows too; NULL means a UI add.
	var tname any
	if taggerName != "" {
		tname = taggerName
	}

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name) VALUES (?, ?, ?, 0, ?, ?)`,
		imageID, tagID, isAutoInt, confidence, tname,
	)
	if err != nil {
		return false, false, fmt.Errorf("inserting image_tag: %w", err)
	}
	added, _ := res.RowsAffected()
	var promoted int64
	if added == 0 {
		// Promote, never demote: implied < auto < manual, so a promoted
		// implied row survives its parent's removal.
		upd, err := tx.Exec(
			`UPDATE image_tags SET is_implied = 0, is_auto = ?, confidence = ?, tagger_name = ?
			 WHERE image_id = ? AND tag_id = ?
			   AND (is_implied = 1 OR (is_auto = 1 AND ? = 0))`,
			isAutoInt, confidence, tname, imageID, tagID, isAutoInt,
		)
		if err != nil {
			return false, false, err
		}
		promoted, _ = upd.RowsAffected()
	} else {
		if err := BumpTagUsageTx(tx, tagID, imageID); err != nil {
			return false, false, err
		}
	}

	// A bare UI re-add of a present tag must not stamp a phantom 'user'
	// source, except over a tag only the derivation claims: Remove meta
	// tags would take a tag the operator typed.
	reclaimed := false
	if added == 0 && promoted == 0 && taggerName == "" {
		if err := tx.QueryRow(
			`SELECT COALESCE(MIN(source) = ?1 AND MAX(source) = ?1, 0) FROM image_tag_sources WHERE image_id = ?2 AND tag_id = ?3`,
			models.TagSourceMonbooru, imageID, tagID,
		).Scan(&reclaimed); err != nil {
			return false, false, err
		}
	}
	if added > 0 || promoted > 0 || taggerName != "" || reclaimed {
		if err := RecordTagSourceTx(tx, imageID, tagID, taggerName); err != nil {
			return false, false, err
		}
	}

	if err := fanOutImpliedTxImpl(tx, imageID, tagID, ratingCatID, isAutoInt); err != nil {
		return false, false, err
	}

	if added == 0 {
		return false, promoted > 0 || reclaimed, nil
	}
	return true, false, nil
}

func TransitiveImpliedTx(tx *sql.Tx, parents []int64) ([]int64, error) {
	if len(parents) == 0 {
		return nil, nil
	}
	var out []int64
	err := bfsImpliedTx(tx, parents, func(id int64) bool {
		out = append(out, id)
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) AddTagsToOneImage(imageID int64, tagIDs []int64, via string) ([]AddResult, error) {
	if len(tagIDs) == 0 {
		return nil, nil
	}
	var results []AddResult
	err := s.inWriteTx(func(tx *sql.Tx) error {
		var err error
		results, err = addTagsTx(tx, imageID, tagIDs, nil, false, via, s.ratingCatID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

type SyncResult struct {
	Added        int
	Retired      int
	RatingFilled bool
}

// A tag the source no longer lists is flagged stale, never removed. Pass
// reconcile false when the site has several posts on the image: they
// share one slice.
func (s *Service) SyncSourceTags(imageID int64, tagIDs []int64, site string, reconcile bool) (SyncResult, error) {
	if site == "" {
		return SyncResult{}, errors.New("source label required")
	}
	incoming := make(map[int64]bool, len(tagIDs))
	for _, id := range tagIDs {
		incoming[id] = true
	}
	var out SyncResult
	err := s.inWriteTx(func(tx *sql.Tx) error {
		alreadyRated := false
		if s.ratingCatID != 0 {
			if err := tx.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM image_tags it JOIN tags t ON t.id = it.tag_id
				 WHERE it.image_id = ? AND t.category_id = ?)`, imageID, s.ratingCatID).Scan(&alreadyRated); err != nil {
				return err
			}
		}

		var res SyncResult
		var applied []int64
		for _, tagID := range tagIDs {
			isRating := false
			if s.ratingCatID != 0 {
				var cat int64
				switch err := tx.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, tagID).Scan(&cat); {
				case errors.Is(err, sql.ErrNoRows):
					continue
				case err != nil:
					return err
				}
				isRating = cat == s.ratingCatID
			}
			if isRating && alreadyRated {
				continue
			}
			added, err := addSourceTagTx(tx, imageID, tagID, site)
			if err != nil {
				return err
			}
			applied = append(applied, tagID)
			if added {
				res.Added++
				if isRating {
					res.RatingFilled = true
					// A payload with several ratings (a PTR hash can
					// carry them) lands only the first.
					alreadyRated = true
				}
			}
		}
		// After every insert: a parent fanned out first would leave its listed child implied.
		for _, tagID := range applied {
			if err := fanOutImpliedTxImpl(tx, imageID, tagID, s.ratingCatID, 0); err != nil {
				return err
			}
		}

		for _, tagID := range tagIDs {
			if _, err := tx.Exec(
				`UPDATE image_tags SET stale = 0
				 WHERE image_id = ? AND tag_id = ? AND tagger_name = ? AND stale = 1`,
				imageID, tagID, site); err != nil {
				return err
			}
		}
		if !reconcile {
			out = res
			return nil
		}
		// Never flag a rating: the image's rating is protected from the source.
		ratingCat := s.ratingCatID
		if ratingCat == 0 {
			ratingCat = -1
		}
		current, err := db.QueryIDs(tx,
			`SELECT it.tag_id FROM image_tags it JOIN tags t ON t.id = it.tag_id
			 WHERE it.image_id = ? AND it.tagger_name = ? AND it.is_auto = 0
			   AND it.stale = 0 AND t.category_id != ?`,
			imageID, site, ratingCat)
		if err != nil {
			return err
		}
		var stale []int64
		for _, tid := range current {
			if !incoming[tid] {
				stale = append(stale, tid)
			}
		}
		for _, tid := range stale {
			if _, err := tx.Exec(
				`UPDATE image_tags SET stale = 1 WHERE image_id = ? AND tag_id = ?`,
				imageID, tid); err != nil {
				return err
			}
			res.Retired++
		}
		out = res
		return nil
	})
	return out, err
}

// Insert-only: an existing row keeps its attribution, so the stale
// flagging never reaches a row this source didn't add.
func addSourceTagTx(tx *sql.Tx, imageID, tagID int64, site string) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name) VALUES (?, ?, 0, 0, NULL, ?)`,
		imageID, tagID, site,
	)
	if err != nil {
		return false, fmt.Errorf("inserting image_tag: %w", err)
	}
	added, _ := res.RowsAffected()
	if added > 0 {
		if err := BumpTagUsageTx(tx, tagID, imageID); err != nil {
			return false, err
		}
	}
	if err := RecordTagSourceTx(tx, imageID, tagID, site); err != nil {
		return false, err
	}
	return added > 0, nil
}

// ratingsReplaced counts images whose rating the add replaced.
func (s *Service) BatchAddTagsTx(tx *sql.Tx, imageIDs []int64, tagIDs []int64) (added, ratingsReplaced int, err error) {
	for _, imageID := range imageIDs {
		replaced := false
		for _, tagID := range tagIDs {
			a, _, displaced, err := addOneTagTx(tx, imageID, tagID, false, nil, "", s.ratingCatID)
			if err != nil {
				return added, ratingsReplaced, err
			}
			if a {
				added++
			}
			if len(displaced) > 0 {
				replaced = true
			}
		}
		if replaced {
			ratingsReplaced++
		}
	}
	return added, ratingsReplaced, nil
}

// An implied row a parent still justifies is skipped and counted in
// implied, not an error: a batch names tags, not rows.
func (s *Service) BatchRemoveTagsTx(tx *sql.Tx, imageIDs []int64, tagIDs []int64) (removed, implied int, err error) {
	for _, imageID := range imageIDs {
		n, kept, err := removeTagsUnlessImpliedTx(tx, imageID, tagIDs)
		removed += n
		implied += len(kept)
		if err != nil {
			return removed, implied, err
		}
	}
	return removed, implied, nil
}

// A child named with its parent is only held until the parent goes, so
// the held rows are checked again once the others are removed.
func removeTagsUnlessImpliedTx(tx *sql.Tx, imageID int64, tagIDs []int64) (removed int, kept []int64, err error) {
	pass := func(ids []int64) ([]int64, error) {
		var held []int64
		for _, tagID := range ids {
			switch implied, err := impliedByParentOnImage(tx, imageID, tagID); {
			case err != nil:
				return nil, err
			case implied:
				held = append(held, tagID)
				continue
			}
			n, err := removeTagFromImageTx(tx, imageID, tagID)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				removed++
			}
		}
		return held, nil
	}
	held, err := pass(tagIDs)
	if err != nil || len(held) == 0 {
		return removed, nil, err
	}
	kept, err = pass(held)
	return removed, kept, err
}

func (s *Service) RemoveTagFromImage(imageID, tagID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		switch implied, err := impliedByParentOnImage(tx, imageID, tagID); {
		case err != nil:
			return err
		case implied:
			return ErrTagImplied
		}
		_, err := removeTagFromImageTx(tx, imageID, tagID)
		return err
	})
}

func removeTagIDsFromImageTx(tx *sql.Tx, imageID int64, tagIDs []int64) (int, error) {
	removed := 0
	for _, tagID := range tagIDs {
		n, err := removeTagFromImageTx(tx, imageID, tagID)
		removed += n
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func (s *Service) RemoveTagsFromOneImage(imageID int64, tagIDs []int64) error {
	if len(tagIDs) == 0 {
		return nil
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		_, kept, err := removeTagsUnlessImpliedTx(tx, imageID, tagIDs)
		if err == nil && len(kept) > 0 {
			return ErrTagImplied
		}
		return err
	})
}

// BumpTagUsageTx skips a missing image: usage_count counts visible images only.
func BumpTagUsageTx(tx *sql.Tx, tagID, imageID int64) error {
	_, err := tx.Exec(
		`UPDATE tags SET usage_count = usage_count + 1,
		        last_used_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
		 WHERE id = ? AND (SELECT is_missing FROM images WHERE id = ?) = 0`,
		tagID, imageID,
	)
	return err
}

func DropTagUsageTx(tx *sql.Tx, tagID, imageID int64) error {
	_, err := tx.Exec(
		`UPDATE tags SET usage_count = MAX(0, usage_count - 1)
		 WHERE id = ? AND (SELECT is_missing FROM images WHERE id = ?) = 0`,
		tagID, imageID,
	)
	return err
}

func removeTagFromImageTx(tx *sql.Tx, imageID, tagID int64) (int, error) {
	implied, err := TransitiveImpliedTx(tx, []int64{tagID})
	if err != nil {
		return 0, err
	}

	res, err := tx.Exec(
		`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, tagID,
	)
	if err != nil {
		return 0, err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return 0, nil
	}

	if err := DropTagUsageTx(tx, tagID, imageID); err != nil {
		return 0, err
	}
	removed := 1

	n, err := SweepImpliedClosureTx(tx, imageID, implied, tagID)
	return removed + n, err
}

// SweepImpliedClosureTx repeats until a pass drops nothing: a closure row
// can be the only justification for another, in any order within a tier.
// excludeParent never counts as a justifying parent; 0 excludes none.
func SweepImpliedClosureTx(tx *sql.Tx, imageID int64, closure []int64, excludeParent int64) (int, error) {
	removed := 0
	for {
		dropped := false
		for _, impID := range closure {
			var rowImplied int
			err := tx.QueryRow(
				`SELECT is_implied FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, impID,
			).Scan(&rowImplied)
			if err == sql.ErrNoRows {
				continue
			} else if err != nil {
				return removed, err
			}
			if rowImplied != 1 {
				continue
			}
			stillImplied, err := implicationParentsOnImageExcluding(tx, imageID, impID, excludeParent)
			if err != nil {
				return removed, err
			}
			if len(stillImplied) > 0 {
				continue
			}
			if _, err := tx.Exec(
				`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, impID,
			); err != nil {
				return removed, err
			}
			if err := DropTagUsageTx(tx, impID, imageID); err != nil {
				return removed, err
			}
			removed++
			dropped = true
		}
		if !dropped {
			return removed, nil
		}
	}
}

// andWhere is raw SQL starting with AND.
func (s *Service) removeMatchingTx(imageID int64, andWhere string, args ...any) (int, error) {
	removed := 0
	err := s.inWriteTx(func(tx *sql.Tx) error {
		tagIDs, err := db.QueryIDs(tx,
			`SELECT tag_id FROM image_tags WHERE image_id = ? `+andWhere,
			append([]any{imageID}, args...)...)
		if err != nil {
			return err
		}
		removed, err = removeTagIDsFromImageTx(tx, imageID, tagIDs)
		return err
	})
	return removed, err
}

// Implied rows have no tagger_name either, but they are not the operator's.
func (s *Service) RemoveUserTagsFromImage(imageID int64) (int, error) {
	return s.removeMatchingTx(imageID,
		`AND is_auto = 0 AND is_implied = 0 AND (tagger_name IS NULL OR tagger_name = '')`)
}

func (s *Service) RemoveStaleTagsFromImage(imageID int64) (int, error) {
	return s.removeMatchingTx(imageID, `AND stale = 1`)
}

func (s *Service) RemoveSourceTagsFromImage(imageID int64, sources []string, stale string) (int, error) {
	if len(sources) == 0 {
		return 0, nil
	}
	placeholders, args := db.InPlaceholders(sources)
	where := `AND is_auto = 0 AND tagger_name IN (` + placeholders + `)`
	switch stale {
	case "1":
		where += ` AND stale = 1`
	case "0":
		where += ` AND stale = 0`
	}
	return s.removeMatchingTx(imageID, where, args...)
}

// Implied rows go with their parent, which may sit in another category.
func (s *Service) RemoveCategoryTagsFromImage(imageID int64, category string) (int, error) {
	return s.removeMatchingTx(imageID,
		`AND is_implied = 0 AND tag_id IN (
		   SELECT t.id FROM tags t
		   JOIN tag_categories tc ON tc.id = t.category_id
		   WHERE tc.name = ?)`, category)
}

// DropSourceFromImageTags withdraws one source's claim: a tag another
// source also vouches for stays on the image.
func (s *Service) DropSourceFromImageTags(imageID int64, source string, tagIDs []int64) (covered, removed int, err error) {
	err = s.inWriteTx(func(tx *sql.Tx) error {
		// Rows with no ledger entry count under their own tagger_name, as
		// the by-source view lists them.
		claim := `SELECT tag_id FROM (
		            SELECT tag_id FROM image_tag_sources
		             WHERE image_id = ? AND source = ?
		            UNION
		            SELECT it.tag_id FROM image_tags it
		             WHERE it.image_id = ? AND it.is_implied = 0
		               AND COALESCE(NULLIF(it.tagger_name, ''), 'user') = ?
		               AND NOT EXISTS (SELECT 1 FROM image_tag_sources s
		                                WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id))`
		args := []any{imageID, source, imageID, source}
		if len(tagIDs) > 0 {
			placeholders, ids := db.InPlaceholders(tagIDs)
			claim += ` WHERE tag_id IN (` + placeholders + `)`
			args = append(args, ids...)
		}
		claimed, err := db.QueryIDs(tx, claim, args...)
		if err != nil {
			return err
		}
		covered = len(claimed)
		if covered == 0 {
			return nil
		}
		placeholders, ids := db.InPlaceholders(claimed)
		if _, err := tx.Exec(
			`DELETE FROM image_tag_sources
			  WHERE image_id = ? AND source = ? AND tag_id IN (`+placeholders+`)`,
			append([]any{imageID, source}, ids...)...); err != nil {
			return err
		}
		orphaned, err := db.QueryIDs(tx,
			`SELECT it.tag_id FROM image_tags it
			  WHERE it.image_id = ? AND it.is_implied = 0 AND it.tag_id IN (`+placeholders+`)
			    AND NOT EXISTS (SELECT 1 FROM image_tag_sources s
			                     WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id)`,
			append([]any{imageID}, ids...)...)
		if err != nil {
			return err
		}
		removed, err = removeTagIDsFromImageTx(tx, imageID, orphaned)
		return err
	})
	return covered, removed, err
}

func (s *Service) RemoveAutoTagsFromImage(imageID int64, taggerNames []string) (int, error) {
	if len(taggerNames) == 0 {
		return s.removeMatchingTx(imageID, `AND is_auto = 1`)
	}
	placeholders, args := db.InPlaceholders(taggerNames)
	return s.removeMatchingTx(imageID, `AND is_auto = 1 AND tagger_name IN (`+placeholders+`)`, args...)
}

// PruneOrphanedImplied leaves usage_count to the caller: RecalcIDs the
// returned tags.
func (s *Service) PruneOrphanedImplied(ctx context.Context, imageIDs []int64) ([]int64, int, error) {
	seen := map[int64]struct{}{}
	removed := 0
	err := db.Chunked(imageIDs, 500, func(chunk []int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.inWriteTx(func(tx *sql.Tx) error {
			n, err := pruneOrphanedImpliedTx(tx, chunk, seen)
			removed += n
			return err
		})
	})
	if err != nil {
		return tagIDsFromSet(seen), removed, err
	}
	return tagIDsFromSet(seen), removed, nil
}

// Loops because a dropped row can be the only justification for another.
func pruneOrphanedImpliedTx(tx *sql.Tx, imageIDs []int64, seen map[int64]struct{}) (int, error) {
	placeholders, args := db.InPlaceholders(imageIDs)
	type orphan struct{ imageID, tagID int64 }
	removed := 0
	for {
		orphans, err := db.QueryAll(tx, func(rows *sql.Rows) (orphan, error) {
			var o orphan
			err := rows.Scan(&o.imageID, &o.tagID)
			return o, err
		}, `SELECT it.image_id, it.tag_id FROM image_tags it
			 WHERE it.is_implied = 1 AND it.image_id IN (`+placeholders+`)
			   AND NOT EXISTS (SELECT 1 FROM tag_implications ti
			                   JOIN image_tags p ON p.image_id = it.image_id AND p.tag_id = ti.parent_tag_id
			                   WHERE ti.implied_tag_id = it.tag_id)`, args...)
		if err != nil {
			return removed, err
		}
		if len(orphans) == 0 {
			return removed, nil
		}
		for _, o := range orphans {
			if _, err := tx.Exec(
				`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, o.imageID, o.tagID,
			); err != nil {
				return removed, err
			}
			seen[o.tagID] = struct{}{}
			removed++
		}
	}
}

func (s *Service) RemoveAllTagsFromImage(imageID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error { return RemoveAllTagsFromImageTx(tx, imageID) })
}

func RemoveAllTagsFromImageTx(tx *sql.Tx, imageID int64) error {
	tagIDs, err := db.QueryIDs(tx, `SELECT tag_id FROM image_tags WHERE image_id = ?`, imageID)
	if err != nil {
		return err
	}

	if len(tagIDs) > 0 {
		var isMissing int
		if err := tx.QueryRow(`SELECT is_missing FROM images WHERE id = ?`, imageID).Scan(&isMissing); err != nil && err != sql.ErrNoRows {
			return err
		}
		if isMissing == 0 {
			placeholders, args := db.InPlaceholders(tagIDs)
			if _, err := tx.Exec(
				`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id IN (`+placeholders+`)`,
				args...,
			); err != nil {
				return err
			}
		}
	}

	_, err = tx.Exec(`DELETE FROM image_tags WHERE image_id = ?`, imageID)
	return err
}

// The related-images probe keeps only this many of the rarest general
// tags; other categories are distinctive enough to scan uncapped.
const relatedGeneralTagsCap = 15

func RatingRank(name string) int { return slices.Index(RatingLevels, name) }

// A manual add replaces the image's rating even with a lower one; an auto
// add keeps the highest, as search resolves several.
func pruneRatingsAfterAddTx(tx *sql.Tx, ratingCatID, imageID, tagID int64, isAuto bool) ([]string, error) {
	if ratingCatID == 0 {
		return nil, nil
	}
	var catID int64
	if err := tx.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, tagID).Scan(&catID); err != nil || catID != ratingCatID {
		return nil, nil
	}
	if isAuto {
		return nil, PruneLowerRatingsTx(tx, ratingCatID, imageID)
	}
	return pruneOtherRatingsTx(tx, ratingCatID, imageID, tagID)
}

type ratingRow struct {
	tagID int64
	name  string
}

func ratingRowsOnImageTx(tx *sql.Tx, ratingCatID, imageID int64) ([]ratingRow, error) {
	return db.QueryAll(tx, func(rows *sql.Rows) (ratingRow, error) {
		var r ratingRow
		err := rows.Scan(&r.tagID, &r.name)
		return r, err
	}, `SELECT it.tag_id, t.name FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.category_id = ? AND t.is_alias = 0`,
		imageID, ratingCatID)
}

// PruneLowerRatingsTx keeps the highest rating; search's rating: fast
// count assumes one per image.
func PruneLowerRatingsTx(tx *sql.Tx, ratingCatID, imageID int64) error {
	if ratingCatID == 0 {
		return nil
	}
	present, err := ratingRowsOnImageTx(tx, ratingCatID, imageID)
	if err != nil {
		return fmt.Errorf("scan rating rows for prune: %w", err)
	}
	if len(present) <= 1 {
		return nil
	}
	bestRank := -1
	for _, r := range present {
		bestRank = max(bestRank, RatingRank(r.name))
	}
	_, err = pruneRatingsTx(tx, imageID, present, func(r ratingRow) bool {
		return RatingRank(r.name) >= bestRank
	})
	if err != nil {
		return fmt.Errorf("prune lower rating: %w", err)
	}
	return nil
}

func pruneRatingsTx(tx *sql.Tx, imageID int64, present []ratingRow, keep func(ratingRow) bool) ([]string, error) {
	var displaced []string
	for _, r := range present {
		if keep(r) {
			continue
		}
		if _, err := removeTagFromImageTx(tx, imageID, r.tagID); err != nil {
			return displaced, fmt.Errorf("remove rating %d: %w", r.tagID, err)
		}
		displaced = append(displaced, r.name)
	}
	return displaced, nil
}

func pruneOtherRatingsTx(tx *sql.Tx, ratingCatID, imageID, keepTagID int64) ([]string, error) {
	if ratingCatID == 0 {
		return nil, nil
	}
	present, err := ratingRowsOnImageTx(tx, ratingCatID, imageID)
	if err != nil {
		return nil, fmt.Errorf("scan rating rows for overwrite: %w", err)
	}
	displaced, err := pruneRatingsTx(tx, imageID, present, func(r ratingRow) bool {
		return r.tagID == keepTagID
	})
	if err != nil {
		return nil, fmt.Errorf("overwrite prior rating: %w", err)
	}
	return displaced, nil
}

func (s *Service) RatingTagIDsAbove(ceiling string) []int64 {
	if s.ratingCatID == 0 {
		return nil
	}
	rank := RatingRank(ceiling)
	if rank < 0 || rank >= len(RatingLevels)-1 {
		return nil
	}
	above := RatingLevels[rank+1:]
	placeholders, nameArgs := db.InPlaceholders(above)
	args := append([]any{s.ratingCatID}, nameArgs...)
	ids, err := db.QueryIDs(s.db.Read,
		`SELECT id FROM tags WHERE category_id = ? AND name IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return nil
	}
	return ids
}
