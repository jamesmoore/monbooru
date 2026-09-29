package relations

import (
	"context"
	"database/sql"

	"github.com/monbooru/monbooru/internal/db"
)

// PruneQueue drops the rows the current settings would not nominate,
// demoting a both-detector pair to the detector still backing it. Review
// pairs stay: the operator asked for those.
func PruneQueue(ctx context.Context, database *db.DB, opts FindPairsOptions) (int, error) {
	threshold := opts.TagPairThreshold
	if !opts.TagPairs {
		// Above every reachable score, so the tag side fails for every row.
		threshold = 2
	}
	removed := 0
	err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE potential_relation_pairs SET source = ?, score = NULL
			  WHERE source = ? AND COALESCE(score, 0) < ?`,
			SourcePhash, SourceBoth, threshold,
		); err != nil {
			return err
		}
		del := func(where string, args ...any) error {
			res, err := tx.ExecContext(ctx,
				`DELETE FROM potential_relation_pairs WHERE `+where, args...)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			removed += int(n)
			return nil
		}
		if err := del(`source = ? AND COALESCE(score, 0) < ?`, SourceTags, threshold); err != nil {
			return err
		}
		if err := demoteOverDistanceTx(ctx, tx, opts.Distance); err != nil {
			return err
		}
		return del(`source = ? AND distance > ?`, SourcePhash, opts.Distance)
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// Re-keys into the tag band so the queue order holds; per row because the
// mapping is TagPairDistance.
func demoteOverDistanceTx(ctx context.Context, tx *sql.Tx, distance int) error {
	type demotion struct {
		a, b  int64
		score float64
	}
	pending, err := db.QueryAll(tx, func(rows *sql.Rows) (demotion, error) {
		var d demotion
		err := rows.Scan(&d.a, &d.b, &d.score)
		return d, err
	}, `SELECT a_image_id, b_image_id, COALESCE(score, 0)
		   FROM potential_relation_pairs WHERE source = ? AND distance > ?`,
		SourceBoth, distance)
	if err != nil {
		return err
	}
	for _, d := range pending {
		if _, err := tx.ExecContext(ctx,
			`UPDATE potential_relation_pairs SET source = ?, distance = ?
			  WHERE a_image_id = ? AND b_image_id = ?`,
			SourceTags, TagPairDistance(d.score), d.a, d.b,
		); err != nil {
			return err
		}
	}
	return nil
}
