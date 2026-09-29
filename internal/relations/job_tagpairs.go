package relations

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tags"
)

const (
	SourcePhash  = "phash"
	SourceTags   = "tags"
	SourceBoth   = "both"
	SourceReview = "review"
)

// The tag band starts above the phash cap of 12, so the smallest-distance
// order drains pixel matches before tag matches.
const (
	tagPairDistanceBase = 16
	tagPairDistanceSpan = 48
)

// Without a cap one cluster of same-character images buries the rest of
// the queue.
const tagPairTopK = 3

// Measured on a booru library's declared duplicates: under this floor, a
// few rare tags can score high between unrelated images.
const tagPairMinShared = 10

func TagPairDistance(score float64) int {
	score = min(max(score, 0), 1)
	return tagPairDistanceBase + int(math.Round((1-score)*tagPairDistanceSpan))
}

type tagPairCandidate struct {
	imageID int64
	score   float64
}

// Scoring per seed in SQL is quadratic in tag usage, so the index is
// loaded once and dies with the run.
func findTagPairs(ctx context.Context, database *db.DB, threshold float64, progress FindPairsProgress) (int, error) {
	// Counted tags never outnumber raw ones, so images under the floor
	// are skipped at load.
	corpus, err := tags.LoadSimilarityCorpus(database, tagPairMinShared)
	if err != nil {
		return 0, fmt.Errorf("load tag-pair corpus: %w", err)
	}
	postings := buildPostings(corpus)
	matches := make([][]tagPairCandidate, len(corpus))
	scan := newPairScan(len(corpus))
	for i := range corpus {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if progress != nil && i%64 == 0 {
			progress(i, len(corpus), "tag probing")
		}
		scorePairsFrom(corpus, postings, i, threshold, matches, scan)
	}
	added := 0
	// One transaction per candidate would make a big fill thousands of
	// tiny WAL writes.
	const txChunk = 500
	var pending []tagPairInsert
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		tx, err := database.Write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		for _, p := range pending {
			landed, err := storeTagPairTx(tx, p)
			if err != nil {
				return err
			}
			if landed {
				added++
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

	for i := range corpus {
		for _, c := range matches[i] {
			if ctx.Err() != nil {
				return added, ctx.Err()
			}
			p, ok, err := admitTagPair(ctx, database, corpus[i].ID, c)
			if err != nil {
				return added, err
			}
			if !ok {
				continue
			}
			pending = append(pending, p)
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
		progress(len(corpus), len(corpus), "tag probing")
	}
	return added, nil
}

// Flat arrays, not a map of per-tag slices: that costs an allocation per
// tag and scatters the walk across the heap.
type tagPostings struct {
	offsets []int32
	entries []int32
}

func (p *tagPostings) carriers(tagID int32) []int32 {
	if tagID < 0 || int(tagID)+1 >= len(p.offsets) {
		return nil
	}
	return p.entries[p.offsets[tagID]:p.offsets[tagID+1]]
}

// scorable counts what sharedWeight counts: an image under the floor can
// never pair, on either side.
func scorable(img *tags.SimilarityCorpusImage) bool {
	n := 0
	for _, t := range img.Tags {
		if t.Seeds && !t.Implied {
			n++
			if n >= tagPairMinShared {
				return true
			}
		}
	}
	return false
}

func buildPostings(corpus []tags.SimilarityCorpusImage) *tagPostings {
	var maxTag int32
	total := 0
	for i := range corpus {
		if !scorable(&corpus[i]) {
			continue
		}
		for _, t := range corpus[i].Tags {
			if !t.Seeds {
				continue
			}
			if t.TagID > maxTag {
				maxTag = t.TagID
			}
			total++
		}
	}
	p := &tagPostings{offsets: make([]int32, maxTag+2), entries: make([]int32, total)}
	for i := range corpus {
		if !scorable(&corpus[i]) {
			continue
		}
		for _, t := range corpus[i].Tags {
			if t.Seeds {
				p.offsets[t.TagID+1]++
			}
		}
	}
	for k := 1; k < len(p.offsets); k++ {
		p.offsets[k] += p.offsets[k-1]
	}
	fill := make([]int32, len(p.offsets))
	copy(fill, p.offsets)
	for i := range corpus {
		if !scorable(&corpus[i]) {
			continue
		}
		for _, t := range corpus[i].Tags {
			if t.Seeds {
				p.entries[fill[t.TagID]] = int32(i)
				fill[t.TagID]++
			}
		}
	}
	return p
}

// stamp marks the candidates the current seed has seen, so dedup is one
// compare.
type pairScan struct {
	seen    []int32
	partial []float64
	cand    []int32
	ordered []tags.SimilarityTag
	stamp   int32
}

func newPairScan(n int) *pairScan {
	return &pairScan{seen: make([]int32, n), partial: make([]float64, n)}
}

// Each pair is scored once, from its lower index, for both sides' top-K lists.
func scorePairsFrom(corpus []tags.SimilarityCorpusImage, postings *tagPostings, i int, threshold float64, matches [][]tagPairCandidate, scan *pairScan) {
	img := &corpus[i]
	if !scorable(img) {
		return
	}
	floor := sharedFloor(img, threshold)
	prefix, outside := prefixTags(img, floor, scan)
	if len(prefix) == 0 {
		return
	}
	// Shared weight is at most either norm, so only a candidate with a
	// norm in this band can clear the threshold.
	loNorm := threshold * threshold * img.Norm
	hiNorm := img.Norm / (threshold * threshold)
	scan.stamp++
	scan.cand = scan.cand[:0]
	for _, t := range prefix {
		for _, j := range postings.carriers(t.TagID) {
			other := &corpus[j]
			if j <= int32(i) || other.CBZ != img.CBZ {
				continue
			}
			if scan.seen[j] != scan.stamp {
				scan.seen[j] = scan.stamp
				scan.partial[j] = 0
				if other.Norm >= loNorm && other.Norm <= hiNorm {
					scan.cand = append(scan.cand, j)
				}
			}
			scan.partial[j] += t.Weight
		}
	}
	for _, j := range scan.cand {
		other := &corpus[j]
		// The tags outside the prefix add at most their own mass, which
		// settles most candidates without a merge.
		if scan.partial[j]+outside < threshold*math.Sqrt(img.Norm*other.Norm) {
			continue
		}
		shared, n := sharedWeight(img.Tags, other.Tags)
		if n < tagPairMinShared {
			continue
		}
		score := tags.SimilarityScore(shared, img.Norm, other.Norm)
		if score < threshold {
			continue
		}
		matches[i] = insertTopK(matches[i], tagPairCandidate{imageID: other.ID, score: score})
		matches[j] = insertTopK(matches[j], tagPairCandidate{imageID: img.ID, score: score})
	}
}

// The other norm is at least the shared weight, so clearing the threshold
// forces shared >= threshold^2 * norm.
func sharedFloor(img *tags.SimilarityCorpusImage, threshold float64) float64 {
	return threshold * threshold * img.Norm
}

// Every admissible pair shares a prefix tag, so only prefix postings are
// walked. Non-seeding tags stay in the remaining mass: they can still be
// shared.
func prefixTags(img *tags.SimilarityCorpusImage, floor float64, scan *pairScan) (prefix []tags.SimilarityTag, outside float64) {
	scan.ordered = append(scan.ordered[:0], img.Tags...)
	ordered := scan.ordered
	sort.Slice(ordered, func(a, b int) bool { return ordered[a].Weight > ordered[b].Weight })
	remaining := img.Norm
	prefix = ordered[:0]
	for _, t := range ordered {
		if remaining < floor {
			break
		}
		if !t.Seeds {
			continue
		}
		prefix = append(prefix, t)
		remaining -= t.Weight
	}
	return prefix, remaining
}

// Both lists must be sorted by tag id. Popular and implied tags add
// weight but not count, or one parent with thirty implications passes for
// thirty agreements.
func sharedWeight(a, b []tags.SimilarityTag) (float64, int) {
	var sum float64
	n := 0
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i].TagID < b[j].TagID:
			i++
		case a[i].TagID > b[j].TagID:
			j++
		default:
			sum += a[i].Weight
			if a[i].Seeds && !a[i].Implied && !b[j].Implied {
				n++
			}
			i++
			j++
		}
	}
	return sum, n
}

func insertTopK(list []tagPairCandidate, c tagPairCandidate) []tagPairCandidate {
	pos := len(list)
	for pos > 0 {
		prev := list[pos-1]
		if prev.score > c.score || (prev.score == c.score && prev.imageID < c.imageID) {
			break
		}
		pos--
	}
	if pos >= tagPairTopK {
		return list
	}
	list = slices.Insert(list, pos, c)
	if len(list) > tagPairTopK {
		list = list[:tagPairTopK]
	}
	return list
}

type tagPairInsert struct {
	lo, hi int64
	score  float64
}

func admitTagPair(ctx context.Context, database *db.DB, seedID int64, c tagPairCandidate) (tagPairInsert, bool, error) {
	lo, hi := canonicalPair(seedID, c.imageID)
	related, err := pairHasDeclaredRelation(ctx, database, lo, hi)
	if err != nil || related {
		return tagPairInsert{}, false, err
	}
	hidden, err := pairSharesPrivateCollection(ctx, database, lo, hi)
	if err != nil || hidden {
		return tagPairInsert{}, false, err
	}
	return tagPairInsert{lo: lo, hi: hi, score: c.score}, true, nil
}

// Reports only a new row, not an upgrade or a raised score.
func storeTagPairTx(tx *sql.Tx, p tagPairInsert) (bool, error) {
	res, err := tx.Exec(
		`UPDATE potential_relation_pairs SET source = ?, score = ?
		  WHERE a_image_id = ? AND b_image_id = ? AND source = ?`,
		SourceBoth, p.score, p.lo, p.hi, SourcePhash)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, nil
	}
	res, err = tx.Exec(
		`INSERT OR IGNORE INTO potential_relation_pairs
		     (a_image_id, b_image_id, distance, created_at, source, score)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.lo, p.hi, TagPairDistance(p.score), time.Now().UTC().Format(time.RFC3339), SourceTags, p.score)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	// Raise only tag rows: a both row's distance is a real Hamming
	// distance, not the score's.
	_, err = tx.Exec(
		`UPDATE potential_relation_pairs SET score = ?, distance = ?
		  WHERE a_image_id = ? AND b_image_id = ? AND source = ? AND score < ?`,
		p.score, TagPairDistance(p.score), p.lo, p.hi, SourceTags, p.score)
	return false, err
}

// Dropped at admission, not just hidden: a collection's pages share
// nearly every tag and would flood the table.
func pairSharesPrivateCollection(ctx context.Context, database *db.DB, a, b int64) (bool, error) {
	var excluded int
	err := database.Read.QueryRowContext(ctx,
		`SELECT `+collectionPairExclusion("?", "?"), b, a).Scan(&excluded)
	if err != nil {
		return false, err
	}
	return excluded == 0, nil
}

// Must match the collection_hidden triggers' verdict; this one runs at
// admission, before a row exists.
func collectionPairExclusion(aCol, bCol string) string {
	return `NOT EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = ` + bCol + `
		WHERE ca.image_id = ` + aCol + `
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))`
}

// Leaves the queue out, so a pair the phash walk queued can still be upgraded.
func pairHasDeclaredRelation(ctx context.Context, database *db.DB, a, b int64) (bool, error) {
	tx, err := database.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	return pairSettledTx(tx, a, b)
}
