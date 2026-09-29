package tags

import (
	"database/sql"
	"errors"
	"math"
	"sort"

	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/db"
)

var errVisibleCount = errors.New("tags: visible image count unavailable")

const SimilarMaxTagUsage = relatedMaxTagUsage

// A tenth of the library, floored at 3: the absolute bound alone would
// let a tag on half a small library count as evidence.
func evidenceUsageCap(visible int64) int64 { return min(int64(SimilarMaxTagUsage), max(3, visible/10)) }

// Only artist gets a bump: weighting character or copyright up rewards
// "same series" over "same picture".
var categoryWeights = map[string]float64{
	"artist": 3.0,
}

// Seeds is false past evidenceUsageCap, but the tag still scores, since
// dropping it would inflate a truncated set's score. Field order and the
// int32 id keep per-row padding down across the whole library.
type SimilarityTag struct {
	Weight  float64
	TagID   int32
	Seeds   bool
	Implied bool
}

type SimilaritySeed struct {
	ImageID int64
	Tags    []SimilarityTag
	Norm    float64
}

func (s SimilaritySeed) TagIDs() []int64 {
	ids := make([]int64, len(s.Tags))
	for i, t := range s.Tags {
		ids[i] = int64(t.TagID)
	}
	return ids
}

// The cosine of the tag sets as vectors of sqrt(weight): it tops out at 1
// and the clamp only absorbs rounding. Squared weights would let two rare
// shared tags read as identical.
func SimilarityScore(shared, seedNorm, candNorm float64) float64 {
	if seedNorm <= 0 || candNorm <= 0 {
		return 0
	}
	return math.Min(1, shared/math.Sqrt(seedNorm*candNorm))
}

func LoadSimilaritySeed(database *db.DB, imageID int64) (SimilaritySeed, error) {
	seed := SimilaritySeed{ImageID: imageID}
	n, ok := counts.VisibleCount(database)
	if !ok {
		return seed, errVisibleCount
	}
	visible := int64(n)
	if visible <= 0 {
		return seed, nil
	}
	rows, err := database.Read.Query(
		`SELECT it.tag_id, t.usage_count, tc.name, it.is_implied
		   FROM image_tags it
		   JOIN tags t ON t.id = it.tag_id
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE it.image_id = ? AND tc.name != 'meta'
		  ORDER BY it.tag_id`,
		imageID,
	)
	if err != nil {
		return seed, err
	}
	defer func() { _ = rows.Close() }()
	evidence := evidenceUsageCap(visible)
	for rows.Next() {
		var tagID, usage int64
		var category string
		var implied bool
		if err := rows.Scan(&tagID, &usage, &category, &implied); err != nil {
			return seed, err
		}
		w := tagWeight(visible, usage, category)
		if w <= 0 {
			continue
		}
		seed.Tags = append(seed.Tags, SimilarityTag{
			TagID: int32(tagID), Weight: w, Seeds: usage <= evidence, Implied: implied,
		})
		seed.Norm += w
	}
	return seed, rows.Err()
}

type SimilarityCorpusImage struct {
	ID   int64
	CBZ  bool
	Tags []SimilarityTag
	Norm float64
}

func LoadSimilarityCorpus(database *db.DB, minTagCount int) ([]SimilarityCorpusImage, error) {
	n, ok := counts.VisibleCount(database)
	if !ok {
		return nil, errVisibleCount
	}
	visible := int64(n)
	if visible <= 0 {
		return nil, nil
	}
	// Weights and eligibility are read once; image_tags streams in key
	// order rather than repeating the same lookups per row.
	weights, countedRows, err := loadTagWeights(database, visible)
	if err != nil {
		return nil, err
	}
	eligible, err := loadScorableImages(database, minTagCount)
	if err != nil {
		return nil, err
	}
	rows, err := database.Read.Query(
		`SELECT image_id, tag_id, is_implied FROM image_tags ORDER BY image_id, tag_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	// One arena keeps the hot walk cache-friendly. Slices are cut only
	// after the fill, since growth moves the arena; sizing it from the
	// counted rows avoids growing at all.
	var corpus []SimilarityCorpusImage
	arena := make([]SimilarityTag, 0, countedRows)
	starts := make([]int, 0, len(eligible))
	var current int64 = -1
	keep := false
	for rows.Next() {
		var imageID, tagID int64
		var implied bool
		if err := rows.Scan(&imageID, &tagID, &implied); err != nil {
			return nil, err
		}
		if imageID != current {
			current = imageID
			cbz, ok := eligible[imageID]
			keep = ok
			if keep {
				corpus = append(corpus, SimilarityCorpusImage{ID: imageID, CBZ: cbz})
				starts = append(starts, len(arena))
			}
		}
		if !keep {
			continue
		}
		t, ok := weights[tagID]
		if !ok || t.Weight <= 0 {
			continue
		}
		t.Implied = implied
		arena = append(arena, t)
		corpus[len(corpus)-1].Norm += t.Weight
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range corpus {
		end := len(arena)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		corpus[i].Tags = arena[starts[i]:end:end]
	}
	return corpus, nil
}

// usage_count counts visible carriers, so the total bounds the corpus arena.
func loadTagWeights(database *db.DB, visible int64) (map[int64]SimilarityTag, int, error) {
	rows, err := database.Read.Query(
		`SELECT t.id, t.usage_count, tc.name FROM tags t
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE tc.name != 'meta'`)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	weights := make(map[int64]SimilarityTag)
	evidence := evidenceUsageCap(visible)
	rowTotal := 0
	for rows.Next() {
		var tagID, usage int64
		var category string
		if err := rows.Scan(&tagID, &usage, &category); err != nil {
			return nil, 0, err
		}
		w := tagWeight(visible, usage, category)
		if w <= 0 {
			continue
		}
		weights[tagID] = SimilarityTag{TagID: int32(tagID), Weight: w, Seeds: usage <= evidence}
		rowTotal += int(usage)
	}
	return weights, rowTotal, rows.Err()
}

func loadScorableImages(database *db.DB, minTagCount int) (map[int64]bool, error) {
	rows, err := database.Read.Query(
		`SELECT id, file_type FROM images WHERE is_missing = 0 AND tag_count >= ?`, minTagCount)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		var fileType string
		if err := rows.Scan(&id, &fileType); err != nil {
			return nil, err
		}
		out[id] = fileType == "cbz"
	}
	return out, rows.Err()
}

type OverlapSeed struct {
	ImageID  int64
	TagIDs   []int64
	MaxUsage int64
}

// LoadOverlapSeed caps usage one short of the library: a tag every image
// carries says nothing.
func LoadOverlapSeed(database *db.DB, imageID int64) (OverlapSeed, error) {
	seed := OverlapSeed{ImageID: imageID}
	n, ok := counts.VisibleCount(database)
	if !ok {
		return seed, errVisibleCount
	}
	seed.MaxUsage = min(int64(SimilarMaxTagUsage), int64(n)-1)
	if seed.MaxUsage < 1 {
		return seed, nil
	}
	ids, err := db.QueryIDs(database.Read,
		`SELECT it.tag_id
		   FROM image_tags it
		   JOIN tags t ON t.id = it.tag_id
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE it.image_id = ? AND tc.name != 'meta' AND t.usage_count <= ?
		  ORDER BY it.tag_id`,
		imageID, seed.MaxUsage)
	seed.TagIDs = ids
	return seed, err
}

// OverlapScore is the similar: metric, asking how much two images'
// tagging overlaps; the weighted score asks whether what they share is
// rare enough to mark the same work.
func OverlapScore(shared, seedTags, candidateTags int) float64 {
	total := seedTags + candidateTags
	if total <= 0 {
		return 0
	}
	return 2 * float64(shared) / float64(total)
}

// A candidate's tags must be counted with the filter and cap the seed was
// loaded with.
func countedJoin(alias string) string {
	return " FROM image_tags " + alias +
		" JOIN tags t ON t.id = " + alias + ".tag_id" +
		" JOIN tag_categories tc ON tc.id = t.category_id" +
		" WHERE tc.name != 'meta' AND t.usage_count <= ?"
}

// 2n/(len+n) is the best score n shared tags can reach, computed as the score
// computes it, so rounding can only admit a candidate, never drop one.
func (s OverlapSeed) MinShared(score float64) int {
	for n := 0; n <= len(s.TagIDs); n++ {
		if 2*float64(n)/float64(len(s.TagIDs)+n) >= score {
			return n
		}
	}
	return len(s.TagIDs) + 1
}

// alias must differ from any enclosing image_tags alias.
func (s OverlapSeed) ScoreExpr(imageCol, alias string) (string, []any) {
	placeholders, args := db.InPlaceholders(s.TagIDs)
	expr := "(SELECT 2.0 * sum(CASE WHEN " + alias + ".tag_id IN (" + placeholders + ") THEN 1 ELSE 0 END)" +
		" / (? + count(*))" + countedJoin(alias) +
		" AND " + alias + ".image_id = " + imageCol + ")"
	return expr, append(args, len(s.TagIDs), s.MaxUsage)
}

func OverlapPercentsAgainst(database *db.DB, seedID int64, ids []int64) (map[int64]int, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seed, err := LoadOverlapSeed(database, seedID)
	if err != nil || len(seed.TagIDs) == 0 {
		return nil, err
	}
	tagPlaceholders, tagArgs := db.InPlaceholders(seed.TagIDs)
	idPlaceholders, idArgs := db.InPlaceholders(ids)
	args := append(tagArgs, seed.MaxUsage)
	args = append(args, idArgs...)
	rows, err := database.Read.Query(
		`SELECT it.image_id,
		        sum(CASE WHEN it.tag_id IN (`+tagPlaceholders+`) THEN 1 ELSE 0 END),
		        count(*)`+countedJoin("it")+`
		    AND it.image_id IN (`+idPlaceholders+`)
		  GROUP BY it.image_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]int, len(ids))
	for rows.Next() {
		var id int64
		var shared, counted int
		if err := rows.Scan(&id, &shared, &counted); err != nil {
			return nil, err
		}
		if pct := int(math.Round(OverlapScore(shared, len(seed.TagIDs), counted) * 100)); pct > 0 {
			out[id] = pct
		}
	}
	return out, rows.Err()
}

type SharedTag struct {
	Name     string
	Category string
	Color    string
	Weight   float64
}

// SharedTags also returns how many tags are shared before the limit.
func SharedTags(database *db.DB, a, b int64, limit int) ([]SharedTag, int, error) {
	seed, err := LoadSimilaritySeed(database, a)
	if err != nil || len(seed.Tags) == 0 {
		return nil, 0, err
	}
	weights := make(map[int64]float64, len(seed.Tags))
	for _, t := range seed.Tags {
		weights[int64(t.TagID)] = t.Weight
	}
	placeholders, args := db.InPlaceholders(seed.TagIDs())
	shared, err := db.QueryAll(database.Read, func(rows *sql.Rows) (SharedTag, error) {
		var tagID int64
		var name, category, color string
		if err := rows.Scan(&tagID, &name, &category, &color); err != nil {
			return SharedTag{}, err
		}
		return SharedTag{
			Name:     name,
			Category: category,
			Color:    SafeCategoryColor(color),
			Weight:   weights[tagID],
		}, nil
	},
		`SELECT it.tag_id, t.name, COALESCE(tc.name, ''), COALESCE(tc.color, '')
		   FROM image_tags it
		   JOIN tags t ON t.id = it.tag_id
		   LEFT JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE it.image_id = ? AND it.tag_id IN (`+placeholders+`)`,
		append([]any{b}, args...)...)
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(shared, func(i, j int) bool {
		if shared[i].Weight != shared[j].Weight {
			return shared[i].Weight > shared[j].Weight
		}
		return shared[i].Name < shared[j].Name
	})
	total := len(shared)
	if limit > 0 && total > limit {
		shared = shared[:limit]
	}
	return shared, total, nil
}

func tagWeight(visible, usage int64, category string) float64 {
	if usage <= 0 || usage >= visible {
		return 0
	}
	w := math.Log(float64(visible) / float64(usage))
	if m, ok := categoryWeights[category]; ok {
		w *= m
	}
	return w
}
