package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

func (s *Server) implicationsDialogHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !isHTMXRequest(r) {
		http.Redirect(w, r, fmt.Sprintf("/tags/%d", id), http.StatusSeeOther)
		return
	}
	parent, err := s.tagSvc().GetTag(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	imps, err := s.tagSvc().ListImplications(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	labels := make([]string, 0, len(imps))
	for _, im := range imps {
		labels = append(labels, im.Origin)
	}
	data := map[string]any{
		"Parent":       parent,
		"Implications": imps,
		"CSRFToken":    s.csrfToken(sessionFromContext(r.Context())),
		"OriginKinds":  s.originKinds(labels),
	}
	s.renderTemplate(w, "partials/implications_dialog.html", data)
}

// ok false means the response is written; ok true means the caller holds
// the job lane for the fan-out. The lane is claimed before the first
// write, so no edge is stored whose fan-out cannot run.
func (s *Server) declareImplications(w http.ResponseWriter, r *http.Request, field string, edge func(tagID int64) (parent, implied int64)) (added int, edges []models.Implication, failures []string, ok bool) {
	raw := strings.TrimSpace(r.FormValue(field))
	if raw == "" {
		writeInlineFlash(w, "err", "Tag name is required.")
		return 0, nil, nil, false
	}
	catTags, _, parseErrMsg := s.parseTagInput(raw)
	if parseErrMsg != "" {
		writeInlineFlash(w, "err", parseErrMsg)
		return 0, nil, nil, false
	}
	if !s.startJob(w, models.JobTypeTag) {
		return 0, nil, nil, false
	}
	for _, ct := range catTags {
		tag, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
		if err != nil {
			failures = append(failures, ct.name+": "+err.Error())
			continue
		}
		parent, implied := edge(tag.ID)
		isNew, err := s.tagSvc().AddImplication(parent, implied)
		if err != nil {
			failures = append(failures, ct.name+": "+err.Error())
			continue
		}
		if isNew {
			added++
		}
		// Re-declaring an edge re-applies it, repairing images that lack what it implies.
		edges = append(edges, models.Implication{ParentID: parent, ImpliedID: implied})
	}
	if added > 0 {
		// GetOrCreateTag may have created tags the cached tag count misses.
		s.active().InvalidateCaches()
	}
	return added, edges, failures, true
}

func (s *Server) propagateDeclared(edges []models.Implication, op string) {
	if len(edges) == 0 {
		s.jobs.Complete("no implication to propagate")
		return
	}
	go s.runImplicationEdges(edges, op)
}

func implicationsAddedMsg(added int) string {
	noun := "implication"
	if added != 1 {
		noun = "implications"
	}
	return strconv.Itoa(added) + " " + noun + " added."
}

func writeImplicationFailures(w http.ResponseWriter, added, declared int, failures []string) bool {
	switch {
	case len(failures) == 0 && added > 0:
		return false
	case len(failures) == 0 && declared == 1:
		writeInlineFlash(w, "ok", "Already declared; re-applying it.")
	case len(failures) == 0:
		writeInlineFlash(w, "ok", "Already declared; re-applying them.")
	case added > 0:
		writeInlineFlash(w, "err", "Added "+strconv.Itoa(added)+". Failed: "+strings.Join(failures, "; "))
	default:
		writeInlineFlash(w, "err", strings.Join(failures, "; "))
	}
	return true
}

func (s *Server) addImplicationPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	parentID, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	added, edges, failures, ok := s.declareImplications(w, r, "implied_id",
		func(tagID int64) (int64, int64) { return parentID, tagID })
	if !ok {
		return
	}
	s.propagateDeclared(edges, "add")
	if added > 0 {
		setFlashHeader(w, implicationsAddedMsg(added), "ok", map[string]any{"implication-added": ""})
	}
	if !writeImplicationFailures(w, added, len(edges), failures) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) addImpliedByPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	impliedID, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	added, edges, failures, ok := s.declareImplications(w, r, "parent_id",
		func(tagID int64) (int64, int64) { return tagID, impliedID })
	if !ok {
		return
	}
	s.propagateDeclared(edges, "add")
	if !writeImplicationFailures(w, added, len(edges), failures) {
		hxDone(w, r, implicationsAddedMsg(added), "", fmt.Sprintf("/tags/%d", impliedID))
	}
}

func (s *Server) removeImplicationDelete(w http.ResponseWriter, r *http.Request) {
	parentID, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	impliedID, ok := pathInt64(w, r, "impliedID")
	if !ok {
		return
	}
	// Claimed before the edge goes: without its sweep the implied rows
	// would stay on the images.
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	if err := s.tagSvc().RemoveImplication(parentID, impliedID); err != nil {
		s.jobs.Fail(err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.propagateDeclared([]models.Implication{{ParentID: parentID, ImpliedID: impliedID}}, "remove")
	setFlashHeader(w, "Implication removed.", "ok",
		map[string]any{"tag-relations-changed": ""})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeImplicationsDelete(w http.ResponseWriter, r *http.Request) {
	s.removeImplicationGroup(w, r, s.tagSvc().ListImplications)
}

func (s *Server) removeImpliedByDelete(w http.ResponseWriter, r *http.Request) {
	s.removeImplicationGroup(w, r, s.tagSvc().ImpliedBy)
}

// One sweep job for the group: a job per edge would be refused after the
// first and leave implied rows behind.
func (s *Server) removeImplicationGroup(w http.ResponseWriter, r *http.Request, list func(int64) ([]models.Implication, error)) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	origin, stale := relationGroupFilter(r)
	edges, err := list(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	var removed []models.Implication
	for _, im := range edges {
		if im.Origin != origin || im.Stale != stale {
			continue
		}
		if err := s.tagSvc().RemoveImplication(im.ParentID, im.ImpliedID); err != nil {
			logx.Warnf("remove implication %d -> %d: %v", im.ParentID, im.ImpliedID, err)
			continue
		}
		removed = append(removed, im)
	}
	if len(removed) > 0 {
		go s.runImplicationGroupSweep(removed)
	} else {
		s.jobs.Complete("no implication to propagate")
	}
	noun := "implication"
	if len(removed) != 1 {
		noun = "implications"
	}
	setFlashHeader(w, strconv.Itoa(len(removed))+" "+noun+" removed.", "ok",
		map[string]any{"tag-relations-changed": ""})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runImplicationGroupSweep(edges []models.Implication) {
	ctx := s.jobs.Context()
	total := len(edges)
	closures := map[int64][]int64{}
	processed := 0
	cancelled := false

	s.jobs.Update(0, total, "removing implications…")
	for i, im := range edges {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		if _, ok := closures[im.ImpliedID]; !ok {
			closure, err := s.resolveRemoveClosure(im.ImpliedID)
			if err != nil {
				s.jobs.Fail(err.Error())
				return
			}
			closures[im.ImpliedID] = closure
		}
		if err := s.sweepImplicationRemovalInline(ctx, im.ParentID, closures[im.ImpliedID]); err != nil {
			logx.Warnf("implication sweep %d -> %d: %v", im.ParentID, im.ImpliedID, err)
		}
		processed = i + 1
		s.jobs.Update(processed, total, "removing implications…")
	}

	implied := make([]int64, 0, len(closures))
	for tagID := range closures {
		implied = append(implied, tagID)
	}
	if err := s.tagSvc().RecalcIDs(implied); err != nil {
		logx.Warnf("implication group sweep recalc: %v", err)
	}
	s.active().InvalidateCaches()
	s.finishJob(nil, cancelled,
		fmt.Sprintf("implication sweep cancelled (%d/%d)", processed, total),
		fmt.Sprintf("swept %d removed implication(s)", processed))
}

// Resolved once, outside the writer-held chunks: the closure does not
// change during a sweep.
func (s *Server) resolveRemoveClosure(tagID int64) ([]int64, error) {
	tx, err := s.db().Read.Begin()
	if err != nil {
		return nil, err
	}
	closure, err := tags.TransitiveImpliedTx(tx, []int64{tagID})
	_ = tx.Rollback()
	if err != nil {
		return nil, err
	}
	return append([]int64{tagID}, closure...), nil
}

func (s *Server) imageIDsWithTag(ctx context.Context, tagID int64) ([]int64, error) {
	return db.QueryIDsContext(ctx, s.db().Read,
		`SELECT image_id FROM image_tags WHERE tag_id = ? ORDER BY image_id`, tagID)
}

func (s *Server) chunkImageTagsByParent(ctx context.Context, parentID int64, perImage func(*sql.Tx, int64) error) error {
	ids, err := s.imageIDsWithTag(ctx, parentID)
	if err != nil {
		return err
	}
	const chunkSize = 500
	for start := 0; start < len(ids); start += chunkSize {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		for _, imageID := range ids[start:min(start+chunkSize, len(ids))] {
			if err := perImage(tx, imageID); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// One job for the whole request: jobs.Start refuses a second, so a job
// per edge would drop every fan-out after the first.
func (s *Server) runImplicationEdges(edges []models.Implication, op string) {
	ctx := s.jobs.Context()
	verb := "applying implication"
	if op == "remove" {
		verb = "removing implication"
	}
	var processed, total int
	cancelled := false
	implied := make([]int64, 0, len(edges))
	// Edges from one parent, or parents on one image, walk an image more than once.
	reached := map[int64]struct{}{}
	for _, e := range edges {
		walked, seen, stopped, err := s.propagateImplicationEdge(ctx, e.ParentID, e.ImpliedID, op, verb)
		processed += len(walked)
		total += seen
		for _, id := range walked {
			reached[id] = struct{}{}
		}
		implied = append(implied, e.ImpliedID)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		if stopped {
			cancelled = true
			break
		}
	}
	if !cancelled {
		if err := s.tagSvc().RecalcIDs(implied); err != nil {
			logx.Warnf("implication propagation recalc: %v", err)
		}
		s.active().InvalidateCaches()
	}
	done := fmt.Sprintf("Implication applied to %d image(s).", len(reached))
	if op == "remove" {
		done = fmt.Sprintf("Implication removed from %d image(s).", len(reached))
	}
	s.finishJob(nil, cancelled, fmt.Sprintf("%s cancelled (%d/%d)", verb, processed, total), done)
}

// The caller writes the job's terminal state, so several edges can share
// one job.
func (s *Server) propagateImplicationEdge(ctx context.Context, parentID, impliedID int64, op, verb string) (walked []int64, total int, cancelled bool, err error) {
	const chunkSize = 500

	ids, err := s.imageIDsWithTag(ctx, parentID)
	if err != nil {
		return nil, 0, false, err
	}

	var removeClosure []int64
	if op == "remove" {
		removeClosure, err = s.resolveRemoveClosure(impliedID)
		if err != nil {
			return nil, len(ids), false, err
		}
	}

	processed, cancelled, err := jobs.Chunked(ctx, s.jobs, ids, chunkSize, verb, func(chunk []int64) error {
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		ratingCatID := s.tagSvc().RatingCategoryID()
		for _, imageID := range chunk {
			if op == "add" {
				if err := propagateAddImplication(tx, imageID, parentID, ratingCatID); err != nil {
					_ = tx.Rollback()
					return err
				}
			} else {
				if err := propagateRemoveImplication(tx, imageID, removeClosure); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
		}
		return tx.Commit()
	})
	return ids[:processed], len(ids), cancelled, err
}

func propagateAddImplication(tx *sql.Tx, imageID, parentID, ratingCatID int64) error {
	var isAuto int
	err := tx.QueryRow(
		`SELECT is_auto FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, parentID,
	).Scan(&isAuto)
	if err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	return tags.ApplyImpliedFanoutTx(tx, imageID, parentID, ratingCatID, isAuto == 1)
}

func propagateRemoveImplication(tx *sql.Tx, imageID int64, closure []int64) error {
	_, err := tags.SweepImpliedClosureTx(tx, imageID, closure, 0)
	return err
}
