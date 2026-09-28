package relations

import (
	"database/sql"

	"github.com/monbooru/monbooru/internal/logx"
)

const DefaultSessionOrder = "smallest_distance_first"

// The trigger-maintained flag spares queue scans a per-row membership
// probe. Expects the queue aliased p.
const collectionPairExcl = "p.collection_hidden = 0"

type PairSide struct {
	ID            int64
	CanonicalPath string
	Width         sql.NullInt64
	Height        sql.NullInt64
	FileSize      int64
	FileType      string
}

type QueuedPair struct {
	A, B     PairSide
	Distance int
	Source   string
	Score    float64
}

type QueueCounts struct {
	Open            int
	HiddenByCeiling int
	Skipped         int
}

func (s *Service) SessionOrder() string {
	var mode string
	if err := s.db.Read.QueryRow(`SELECT order_mode FROM relation_session WHERE id = 1`).Scan(&mode); err != nil {
		return DefaultSessionOrder
	}
	return mode
}

func (s *Service) SetSessionOrder(mode string) {
	if _, err := s.db.Write.Exec(
		`INSERT INTO relation_session (id, order_mode) VALUES (1, ?)
		 ON CONFLICT(id) DO UPDATE SET order_mode = excluded.order_mode`,
		mode,
	); err != nil {
		logx.Debugf("save session order: %v", err)
	}
}

// SkipPair keeps the pair queued but out of the walk until ResetSkipped,
// so a skip always moves forward.
func (s *Service) SkipPair(a, b int64, at string) error {
	_, err := s.db.Write.Exec(
		`UPDATE potential_relation_pairs SET skipped_at = ? WHERE a_image_id = ? AND b_image_id = ?`,
		at, a, b,
	)
	return err
}

func (s *Service) DropQueuedPair(a, b int64) error {
	_, err := s.db.Write.Exec(
		`DELETE FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`, a, b,
	)
	return err
}

// A reopened (review) pair shows in every scope: the operator asked for it.
func detectorFilter(mode string) string {
	switch mode {
	case "phash":
		return " AND p.source IN ('phash', 'both', 'review')"
	case "tags":
		return " AND p.source IN ('tags', 'both', 'review')"
	}
	return ""
}

func orderClauseForMode(mode string) string {
	const base = "ORDER BY "
	switch mode {
	case "largest_file_first":
		return base + "(COALESCE(ia.file_size, 0) + COALESCE(ib.file_size, 0)) DESC, p.distance ASC, p.a_image_id ASC"
	case "random":
		return base + "random()"
	}
	return base + "p.distance ASC, (COALESCE(ia.file_size, 0) + COALESCE(ib.file_size, 0)) DESC, p.a_image_id ASC"
}

// The stored pair rank spares the counts and the pick a per-row
// image_tags probe.
func ceilingClause(rank *int) (string, []any) {
	if rank == nil {
		return "", nil
	}
	return "p.max_rating_rank <= ?", []any{*rank}
}

func (s *Service) QueueCountsFor(detector string, rank *int) (QueueCounts, error) {
	where, args := ceilingClause(rank)
	openExpr := "p.skipped_at IS NULL"
	if where != "" {
		openExpr += " AND " + where
	}
	var counts QueueCounts
	var unskipped int
	err := s.db.Read.QueryRow(`
		SELECT COALESCE(SUM(CASE WHEN p.skipped_at IS NULL THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN `+openExpr+` THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN p.skipped_at IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM potential_relation_pairs p
		WHERE `+collectionPairExcl+detectorFilter(detector), args...,
	).Scan(&unskipped, &counts.Open, &counts.Skipped)
	counts.HiddenByCeiling = unskipped - counts.Open
	return counts, err
}

const queuedPairSelect = `
	SELECT p.a_image_id, p.b_image_id, p.distance, p.source, COALESCE(p.score, 0),
	       ia.canonical_path, COALESCE(ia.width, 0), COALESCE(ia.height, 0), ia.file_size, ia.file_type,
	       ib.canonical_path, COALESCE(ib.width, 0), COALESCE(ib.height, 0), ib.file_size, ib.file_type
	FROM potential_relation_pairs p
	JOIN images ia ON ia.id = p.a_image_id
	JOIN images ib ON ib.id = p.b_image_id`

// NextPair serves a pinned pair whatever its scope or skip, since the
// operator clicked it, and falls back to the ordered pick once it is gone.
func (s *Service) NextPair(order, detector string, rank *int, pinA, pinB int64) (*QueuedPair, error) {
	where, args := ceilingClause(rank)
	scope := detectorFilter(detector)

	ordered := queuedPairSelect + "\n\tWHERE " + collectionPairExcl + scope + " AND p.skipped_at IS NULL"
	if where != "" {
		ordered += " AND " + where
	}
	ordered += "\n\t" + orderClauseForMode(order) + "\n\tLIMIT 1"

	query, qargs := ordered, args
	if pinA > 0 && pinB > 0 {
		lo, hi := canonicalPair(pinA, pinB)
		pinned := queuedPairSelect + "\n\tWHERE " + collectionPairExcl
		if where != "" {
			pinned += " AND " + where
		}
		pinned += " AND p.a_image_id = ? AND p.b_image_id = ?\n\tLIMIT 1"
		query, qargs = pinned, append(append([]any{}, args...), lo, hi)
	}

	pair, err := s.scanQueuedPair(query, qargs...)
	if err == sql.ErrNoRows && query != ordered {
		pair, err = s.scanQueuedPair(ordered, args...)
	}
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return pair, err
}

func (s *Service) scanQueuedPair(query string, args ...any) (*QueuedPair, error) {
	var p QueuedPair
	err := s.db.Read.QueryRow(query, args...).Scan(
		&p.A.ID, &p.B.ID, &p.Distance, &p.Source, &p.Score,
		&p.A.CanonicalPath, &p.A.Width, &p.A.Height, &p.A.FileSize, &p.A.FileType,
		&p.B.CanonicalPath, &p.B.Width, &p.B.Height, &p.B.FileSize, &p.B.FileType,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Service) QueueBySource(rank *int) (open, skipped int, bySource map[string]int, err error) {
	query := `SELECT p.source, p.skipped_at IS NOT NULL, COUNT(*)
		FROM potential_relation_pairs p WHERE ` + collectionPairExcl
	where, args := ceilingClause(rank)
	if where != "" {
		query += " AND " + where
	}
	query += ` GROUP BY p.source, p.skipped_at IS NOT NULL`

	rows, err := s.db.Read.Query(query, args...)
	if err != nil {
		return 0, 0, nil, err
	}
	defer func() { _ = rows.Close() }()
	bySource = map[string]int{}
	for rows.Next() {
		var source string
		var isSkipped bool
		var n int
		if err := rows.Scan(&source, &isSkipped, &n); err != nil {
			return 0, 0, nil, err
		}
		if isSkipped {
			skipped += n
			continue
		}
		open += n
		bySource[source] += n
	}
	return open, skipped, bySource, rows.Err()
}
