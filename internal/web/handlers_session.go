package web

import (
	"database/sql"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/tags"
)

var validOrderModes = map[string]bool{
	"smallest_distance_first": true,
	"largest_file_first":      true,
	"random":                  true,
}

var validDetectors = map[string]bool{
	"phash": true,
	"tags":  true,
	"both":  true,
}

type sessionPairView struct {
	A              sessionImageView
	B              sessionImageView
	Distance       int
	Remaining      int
	Order          string
	Source         string
	Score          float64
	LeftID         int64
	SharedAncestor int64
}

func (v sessionPairView) ScorePercent() int { return int(math.Round(v.Score * 100)) }

func (v sessionPairView) FromTags() bool {
	return v.Source == relations.SourceTags || v.Source == relations.SourceBoth
}

type sessionImageView struct {
	ID       int64
	Width    sql.NullInt64
	Height   sql.NullInt64
	FileSize int64
	Filename string
	FileType string
	TagCount int
}

func (s *Server) sessionPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	order := r.URL.Query().Get("order")
	if order == "" {
		order = cx.RelationsSvc.SessionOrder()
	}
	if !validOrderModes[order] {
		order = "smallest_distance_first"
	}
	if validOrderModes[r.URL.Query().Get("order")] {
		// Only a valid ?order= is saved, so a bogus one cannot replace
		// the stored mode with the fallback.
		cx.RelationsSvc.SetSessionOrder(order)
	}
	// Unlike the order, the scope is not stored: it lasts one sitting.
	tagPairs := s.tagPairsEnabled()
	detector := "phash"
	if tagPairs {
		detector = "both"
		if v := r.URL.Query().Get("detector"); validDetectors[v] {
			detector = v
		}
	}
	ceiling := resolveCeiling(r, cx)
	pinA, _ := strconv.ParseInt(r.URL.Query().Get("a"), 10, 64)
	pinB, _ := strconv.ParseInt(r.URL.Query().Get("b"), 10, 64)
	pair, counts, err := loadNextPair(cx, order, detector, ceiling, pinA, pinB)
	if err != nil {
		logx.Warnf("session next pair: %v", err)
		http.Error(w, "load pair", http.StatusInternalServerError)
		return
	}
	var leftFacts, rightFacts relationCompareFacts
	var sharedTags []tags.SharedTag
	var sharedTotal int
	if pair != nil {
		leftID := pair.LeftID
		rightID := pair.A.ID
		if leftID == pair.A.ID {
			rightID = pair.B.ID
		}
		leftFacts, rightFacts, err = loadCompareFacts(cx, leftID, rightID)
		if err != nil {
			logx.Debugf("session compare facts: %v", err)
		}
		if pair.FromTags() {
			shared, total, sErr := tags.SharedTags(cx.DB, leftID, rightID, sharedTagsShown)
			if sErr != nil {
				logx.Debugf("session shared tags: %v", sErr)
			}
			sharedTags, sharedTotal = shared, total
		}
		if src, ok, sErr := relations.CommonDerivativeAncestor(cx.DB, pair.A.ID, pair.B.ID); sErr != nil {
			logx.Debugf("session shared ancestor: %v", sErr)
		} else if ok {
			pair.SharedAncestor = src
		}
	}
	s.renderTemplate(w, "relations_session.html", sessionPageData{
		baseData:        s.base(r, "relations", "Session - "+s.booruName()),
		Pair:            pair,
		Remaining:       counts.Open,
		HiddenByCeiling: counts.HiddenByCeiling,
		Skipped:         counts.Skipped,
		Ceiling:         ceiling.Level(),
		Order:           order,
		Detector:        detector,
		TagPairs:        tagPairs,
		ActiveGallery:   s.activeGallery(),
		Left:            leftFacts,
		Right:           rightFacts,
		SharedTags:      sharedTags,
		SharedTagsTotal: sharedTotal,
	})
}

const sharedTagsShown = 6

type sessionPageData struct {
	baseData
	Pair            *sessionPairView
	Remaining       int
	HiddenByCeiling int
	Skipped         int
	Ceiling         string
	Order           string
	Detector        string
	TagPairs        bool
	ActiveGallery   string
	Left            relationCompareFacts
	Right           relationCompareFacts
	SharedTags      []tags.SharedTag
	SharedTagsTotal int
}

func loadNextPair(cx *galleryCtx, order, detector string, ceiling *Ceiling, pinA, pinB int64) (*sessionPairView, relations.QueueCounts, error) {
	var rank *int
	if r, active := ceiling.RankCeiling(); active {
		rank = &r
	}
	counts, err := cx.RelationsSvc.QueueCountsFor(detector, rank)
	if err != nil {
		return nil, counts, err
	}
	pinned := pinA > 0 && pinB > 0
	if counts.Open == 0 && !pinned {
		return nil, counts, nil
	}
	pair, err := cx.RelationsSvc.NextPair(order, detector, rank, pinA, pinB)
	if err != nil || pair == nil {
		return nil, counts, err
	}

	view := sessionPairView{
		Order: order, Remaining: counts.Open,
		Distance: pair.Distance, Source: pair.Source, Score: pair.Score,
		A: sessionImageView{
			ID: pair.A.ID, Width: pair.A.Width, Height: pair.A.Height,
			FileSize: pair.A.FileSize, FileType: pair.A.FileType,
			Filename: path.Base(pair.A.CanonicalPath), TagCount: countTags(cx, pair.A.ID),
		},
		B: sessionImageView{
			ID: pair.B.ID, Width: pair.B.Width, Height: pair.B.Height,
			FileSize: pair.B.FileSize, FileType: pair.B.FileType,
			Filename: path.Base(pair.B.CanonicalPath), TagCount: countTags(cx, pair.B.ID),
		},
	}
	// The larger file is the likelier original; a tag match is more
	// likely a derivative, so the older image leads - A, as the queue
	// stores a pair in id order.
	view.LeftID = view.A.ID
	if view.Source != relations.SourceTags &&
		(view.B.FileSize > view.A.FileSize ||
			(view.B.FileSize == view.A.FileSize && view.B.ID < view.A.ID)) {
		view.LeftID = view.B.ID
	}
	return &view, counts, nil
}

type relationCompareFacts struct {
	ImageID         int64
	ResolutionW     int64
	ResolutionH     int64
	FileSize        int64
	AddedAt         string
	TagCount        int
	UniqueTags      []compareTag
	UniqueTagsTotal int
	Format          string
	Collection      string
}

type compareTag struct {
	Name     string
	Category string
	Color    string
}

func loadCompareFacts(cx *galleryCtx, leftID, rightID int64) (relationCompareFacts, relationCompareFacts, error) {
	left := relationCompareFacts{ImageID: leftID}
	right := relationCompareFacts{ImageID: rightID}
	if err := scanCompareFacts(cx, leftID, &left); err != nil {
		return left, right, err
	}
	if err := scanCompareFacts(cx, rightID, &right); err != nil {
		return left, right, err
	}
	leftUnique, rightUnique, err := loadTagDelta(cx, leftID, rightID)
	if err != nil {
		return left, right, err
	}
	const shown = 5
	left.UniqueTagsTotal = len(leftUnique)
	right.UniqueTagsTotal = len(rightUnique)
	left.UniqueTags = leftUnique[:min(shown, len(leftUnique))]
	right.UniqueTags = rightUnique[:min(shown, len(rightUnique))]
	return left, right, nil
}

func scanCompareFacts(cx *galleryCtx, id int64, dst *relationCompareFacts) error {
	var w, h sql.NullInt64
	var addedAt, series sql.NullString
	var canonical, fileType string
	if err := cx.DB.Read.QueryRow(
		`SELECT COALESCE(width, 0), COALESCE(height, 0), file_size, ingested_at, canonical_path, file_type, series
		 FROM images WHERE id = ?`, id,
	).Scan(&w, &h, &dst.FileSize, &addedAt, &canonical, &fileType, &series); err != nil {
		return err
	}
	if w.Valid {
		dst.ResolutionW = w.Int64
	}
	if h.Valid {
		dst.ResolutionH = h.Int64
	}
	if addedAt.Valid {
		dst.AddedAt = humanISODate(addedAt.String)
	}
	if dot := strings.LastIndexByte(canonical, '.'); dot >= 0 {
		dst.Format = strings.ToLower(canonical[dot:])
	}
	if series.Valid {
		dst.Collection = series.String
	}
	dst.TagCount = countTags(cx, id)
	return nil
}

func loadTagDelta(cx *galleryCtx, leftID, rightID int64) (left []compareTag, right []compareTag, err error) {
	rows, err := cx.DB.Read.Query(`
		WITH delta AS (
			SELECT it.tag_id, MAX(it.image_id) AS owner_id
			FROM image_tags it
			LEFT JOIN tags t ON t.id = it.tag_id
			LEFT JOIN tag_categories tc ON tc.id = t.category_id
			WHERE it.image_id IN (?, ?)
			  AND (tc.name IS NULL OR tc.name != 'rating')
			GROUP BY it.tag_id
			HAVING COUNT(*) = 1
		)
		SELECT delta.owner_id, t.name, COALESCE(tc.name, ''), COALESCE(tc.color, '')
		FROM delta
		JOIN tags t ON t.id = delta.tag_id
		LEFT JOIN tag_categories tc ON tc.id = t.category_id
		ORDER BY t.name
		LIMIT 200`, leftID, rightID,
	)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var owner int64
		var t compareTag
		var color string
		if scanErr := rows.Scan(&owner, &t.Name, &t.Category, &color); scanErr != nil {
			return nil, nil, scanErr
		}
		t.Color = tags.SafeCategoryColor(color)
		switch owner {
		case leftID:
			left = append(left, t)
		case rightID:
			right = append(right, t)
		}
	}
	return left, right, rows.Err()
}

func countTags(cx *galleryCtx, id int64) int {
	var n int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ?`, id).Scan(&n); err != nil {
		return 0
	}
	return n
}

func (s *Server) sessionDecidePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	a, ok := formInt64(w, r, "a")
	if !ok {
		return
	}
	b, ok := formInt64(w, r, "b")
	if !ok {
		return
	}
	decision := r.FormValue("type")
	left := a
	if raw := r.FormValue("left"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && (v == a || v == b) {
			left = v
		}
	}
	right := a
	if left == a {
		right = b
	}
	now := time.Now().UTC().Format(time.RFC3339)

	if decision == "skip" {
		if err := cx.RelationsSvc.SkipPair(a, b, now); err != nil {
			logx.Warnf("session skip: %v", err)
			http.Error(w, "skip", http.StatusInternalServerError)
			return
		}
		sessionRedirect(w, r)
		return
	}

	// Left is the original, the parent or the source; the symmetric kinds
	// ignore the order.
	var err error
	switch decision {
	case "duplicate":
		err = cx.RelationsSvc.AddDuplicateOf(left, right)
	case "alternate":
		err = cx.RelationsSvc.AddAlternate(left, right)
	case "version":
		err = cx.RelationsSvc.AddVersionEdge(left, right)
	case "derivative":
		err = cx.RelationsSvc.AddDerivativeEdge(left, right)
	case "not_related":
		err = cx.RelationsSvc.AddNotRelated(left, right)
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown decision.")
		return
	}
	if err != nil {
		writeRelationError(w, err)
		return
	}
	if err := cx.RelationsSvc.DropQueuedPair(a, b); err != nil {
		logx.Warnf("session queue drop: %v", err)
	}
	cx.InvalidateCaches()
	if decision == "duplicate" && writeDuplicatePostDecideHeaders(w, cx, left, right) {
		return
	}
	sessionRedirect(w, r)
}

func writeDuplicatePostDecideHeaders(w http.ResponseWriter, cx *galleryCtx, left, right int64) bool {
	var gid, original int64
	if err := cx.DB.Read.QueryRow(`
		SELECT g.id, g.original_image_id
		FROM dup_group_members ma
		JOIN dup_group_members mb ON ma.group_id = mb.group_id
		JOIN dup_groups g ON g.id = ma.group_id
		WHERE ma.image_id = ? AND mb.image_id = ?
		LIMIT 1`, left, right,
	).Scan(&gid, &original); err != nil {
		logx.Debugf("dup post-decide group lookup: %v", err)
		return false
	}
	nonOriginal := right
	var hasUnique int
	if err := cx.DB.Read.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT it.tag_id
			FROM image_tags it
			LEFT JOIN tags t ON t.id = it.tag_id
			LEFT JOIN tag_categories tc ON tc.id = t.category_id
			WHERE it.image_id = ?
			  AND (tc.name IS NULL OR tc.name != 'rating')
			  AND NOT EXISTS (
			    SELECT 1 FROM image_tags it2 WHERE it2.image_id = ? AND it2.tag_id = it.tag_id
			  )
			LIMIT 1
		)`, nonOriginal, original,
	).Scan(&hasUnique); err != nil {
		logx.Debugf("dup post-decide unique tags: %v", err)
	}
	w.Header().Set("X-Relations-Post-Decision", "duplicate-cleanup")
	w.Header().Set("X-Relations-Duplicate-ID", strconv.FormatInt(nonOriginal, 10))
	w.Header().Set("X-Relations-Duplicate-OriginalID", strconv.FormatInt(original, 10))
	w.Header().Set("X-Relations-Duplicate-GroupID", strconv.FormatInt(gid, 10))
	if hasUnique > 0 {
		w.Header().Set("X-Relations-Duplicate-HasUniqueTags", "1")
	} else {
		w.Header().Set("X-Relations-Duplicate-HasUniqueTags", "0")
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}

func sessionRedirect(w http.ResponseWriter, r *http.Request) {
	dest := "/relations/session?order=" + url.QueryEscape(r.FormValue("order"))
	// Carried through, or a decision inside one detector's walk drops
	// back to both.
	if d := r.FormValue("detector"); validDetectors[d] {
		dest += "&detector=" + url.QueryEscape(d)
	}
	hxRedirect(w, r, dest)
}
