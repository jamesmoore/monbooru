package web

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
)

// findRelationPairsPost queues the find-pairs background job against
// the active gallery's BK-tree. Form-encoded knobs:
//   - distance (int, 0..12, default 4) - Hamming distance cap.
//   - replace ("true" / unset) - wipe potential_relation_pairs before
//     re-scanning.
func (s *Server) findRelationPairsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.active()
	if cx == nil || cx.DB == nil || cx.BKTree == nil {
		flashStatus(w, http.StatusInternalServerError, "No active gallery.")
		return
	}
	s.cfgMu.Lock()
	distance := s.cfg.Relations.DefaultDistance
	tagPairs := s.cfg.Relations.TagPairs
	tagPairThreshold := s.cfg.Relations.TagPairThreshold
	s.cfgMu.Unlock()
	if distance < 0 || distance > maxPhashDistance {
		distance = 4
	}
	if v := r.FormValue("distance"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= maxPhashDistance {
			distance = n
		}
	}
	replace := r.FormValue("replace") == "true"
	// replace=true wipes potential_relation_pairs; gate it behind an
	// explicit confirm so a non-HTMX caller (curl, bookmarked URL, broken
	// script) can't drop the queue with a single missing flag.
	if replace && r.FormValue("confirm") != "REBUILD" {
		flashStatus(w, http.StatusBadRequest, "replace=true requires confirm=REBUILD.")
		return
	}

	if !s.startJob(w, models.JobTypeRelations) {
		return
	}
	database := cx.DB
	thumbnailsPath := cx.ThumbnailsPath
	tree := cx.BKTree
	opts := relations.FindPairsOptions{
		Distance:         distance,
		Replace:          replace,
		ThumbnailsPath:   thumbnailsPath,
		TagPairs:         tagPairs,
		TagPairThreshold: config.ClampTagPairThreshold(tagPairThreshold),
	}
	go func() {
		ctx := s.jobs.Context()
		added, err := relations.FindPairs(ctx, database, tree, opts, func(processed, total int, phase string) {
			s.jobs.Update(processed, total, fmt.Sprintf("find-pairs: %s", phase))
		})
		_ = s.settleJob(ctx, err,
			fmt.Sprintf("find-pairs cancelled (%d added)", added),
			fmt.Sprintf("find-pairs added %d candidate(s).", added))
	}()
	writeInlineFlash(w, "ok", "Find-pairs started.")
}

// resetSkippedPost clears skipped_at on every potential_relation_pairs
// row so previously-skipped pairs surface again at the front of the
// queue on the next session render.
func (s *Server) resetSkippedPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.active()
	if cx == nil || cx.DB == nil {
		flashStatus(w, http.StatusInternalServerError, "No active gallery.")
		return
	}
	n, err := cx.RelationsSvc.ResetSkipped(r.Context())
	if err != nil {
		logx.Warnf("reset skipped: %v", err)
		flashStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeInlineFlash(w, "ok", fmt.Sprintf("Reset %d skipped pair(s).", n))
}
