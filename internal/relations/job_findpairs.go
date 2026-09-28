package relations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
)

var IncrementalProbeDistance atomic.Int32

var IncrementalProbeEnabled atomic.Bool

func init() {
	IncrementalProbeDistance.Store(4)
	IncrementalProbeEnabled.Store(true)
}

type FindPairsOptions struct {
	Distance         int
	Replace          bool
	ThumbnailsPath   string
	TagPairs         bool
	TagPairThreshold float64
}

type FindPairsProgress func(processed, total int, phase string)

// FindPairs also computes missing phashes, so it doubles as a phash backfill.
func FindPairs(ctx context.Context, database *db.DB, tree *BKTree, opts FindPairsOptions, progress FindPairsProgress) (added int, err error) {
	if tree == nil {
		return 0, errors.New("relations: nil bk-tree")
	}
	if opts.Replace {
		if _, err := database.Write.ExecContext(ctx, `DELETE FROM potential_relation_pairs`); err != nil {
			return 0, fmt.Errorf("wipe queue: %w", err)
		}
		tree.Reset()
	}

	// Archives stay in the tree but out of the queue: their phash is only
	// the cover page.
	type row struct {
		id       int64
		phash    sql.NullInt64
		fileType string
	}
	entries, err := db.QueryAll(database.Read, func(rows *sql.Rows) (row, error) {
		var r row
		err := rows.Scan(&r.id, &r.phash, &r.fileType)
		return r, err
	}, `SELECT id, phash, file_type FROM images WHERE is_missing = 0 ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("load image ids: %w", err)
	}
	unqueued := make(map[int64]bool)
	for _, r := range entries {
		if r.fileType == models.FileTypeCBZ {
			unqueued[r.id] = true
		}
	}
	// Missing rows stay in the tree for phash search, not in the queue.
	missing, err := db.QueryIDs(database.Read, `SELECT id FROM images WHERE is_missing = 1 AND phash IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("load missing ids: %w", err)
	}
	for _, id := range missing {
		unqueued[id] = true
	}

	if err := tree.EnsureBuilt(database); err != nil {
		return 0, fmt.Errorf("bk-tree build: %w", err)
	}

	total := len(entries)
	now := time.Now().UTC().Format(time.RFC3339)
	const txChunk = 500
	var pending []pairToInsert

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		tx, err := database.Write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT OR IGNORE INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, ?, ?)`)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		for _, p := range pending {
			if _, err := stmt.Exec(p.a, p.b, p.distance, now); err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				return err
			}
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

	// Every phash goes into the tree before any probe: a row pairs only
	// with higher ids, so probing it before its higher-id partner is in
	// would lose the pair.
	for idx := range entries {
		if ctx.Err() != nil {
			return added, ctx.Err()
		}
		if entries[idx].phash.Valid {
			continue
		}
		if progress != nil {
			progress(idx, total, "phashing")
		}
		h, err := gallery.RecomputeAndStorePhash(ctx, database, entries[idx].id, opts.ThumbnailsPath)
		if err != nil {
			logx.Debugf("find-pairs phash %d: %v", entries[idx].id, err)
			continue
		}
		entries[idx].phash = sql.NullInt64{Int64: h, Valid: true}
		tree.Insert(entries[idx].id, h)
	}

	for idx, e := range entries {
		if ctx.Err() != nil {
			if flushErr := flush(); flushErr != nil {
				logx.Debugf("find-pairs flush during cancel: %v", flushErr)
			}
			return added, ctx.Err()
		}
		if !e.phash.Valid {
			continue // phash compute failed in pass 1
		}
		if unqueued[e.id] {
			continue
		}
		if progress != nil && idx%64 == 0 {
			progress(idx, total, "probing")
		}
		candidates, _ := tree.SearchWithinDistance(e.phash.Int64, opts.Distance)
		for _, cid := range candidates {
			if cid <= e.id {
				continue // each pair once, from its lower id
			}
			if unqueued[cid] {
				continue
			}
			already, err := pairAlreadyKnown(ctx, database, e.id, cid)
			if err != nil {
				return added, err
			}
			if already {
				continue
			}
			pending = append(pending, pairToInsert{
				a: e.id, b: cid, distance: hammingDistance(e.phash.Int64, lookupPhashFromTree(tree, cid)),
			})
			added++
			if len(pending) >= txChunk {
				if err := flush(); err != nil {
					return added, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return added, err
	}
	if progress != nil {
		progress(total, total, "probing")
	}
	if opts.TagPairs {
		tagAdded, err := findTagPairs(ctx, database, opts.TagPairThreshold, progress)
		added += tagAdded
		if err != nil {
			return added, err
		}
	}
	return added, nil
}

type pairToInsert struct {
	a, b     int64
	distance int
}

func pairAlreadyKnown(ctx context.Context, database *db.DB, a, b int64) (bool, error) {
	tx, err := database.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if got, err := pairSettledTx(tx, a, b); err != nil {
		return false, err
	} else if got {
		return true, nil
	}
	lo, hi := canonicalPair(a, b)
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// An archive is hashed by its cover page alone (the tag pass pairs
// archives), and a missing image has nothing to show in a session.
func imageUnqueued(database *db.DB, id int64) (bool, error) {
	var fileType string
	var isMissing bool
	if err := database.Read.QueryRow(`SELECT file_type, is_missing FROM images WHERE id = ?`, id).Scan(&fileType, &isMissing); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return fileType == models.FileTypeCBZ || isMissing, nil
}

func lookupPhashFromTree(tree *BKTree, id int64) int64 {
	tree.mu.RLock()
	defer tree.mu.RUnlock()
	return tree.idIndex[id]
}

func incrementalProbe(database *db.DB, tree *BKTree, id, phash int64, distance int) error {
	if unqueued, err := imageUnqueued(database, id); err != nil || unqueued {
		return err
	}
	candidates, _ := tree.SearchWithinDistance(phash, distance)
	if len(candidates) == 0 {
		return nil
	}
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, cid := range candidates {
		if cid == id {
			continue
		}
		if unqueued, err := imageUnqueued(database, cid); err != nil {
			return err
		} else if unqueued {
			continue
		}
		lo, hi := canonicalPair(id, cid)
		known, err := pairAlreadyKnown(ctx, database, lo, hi)
		if err != nil {
			return err
		}
		if known {
			continue
		}
		other := lookupPhashFromTree(tree, cid)
		if _, err := database.Write.Exec(
			`INSERT OR IGNORE INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, ?, ?)`,
			lo, hi, hammingDistance(phash, other), now,
		); err != nil {
			return err
		}
	}
	return nil
}
