package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
)

// Capped at three: the job summary is one line and the only place they show.
type skipReasons struct {
	seen    []string
	dropped int
}

func (s *skipReasons) add(err error) {
	msg := err.Error()
	if slices.Contains(s.seen, msg) {
		return
	}
	if len(s.seen) >= 3 {
		s.dropped++
		return
	}
	s.seen = append(s.seen, msg)
}

func (s *skipReasons) any() bool { return len(s.seen) > 0 }

func (s *skipReasons) String() string {
	out := strings.Join(s.seen, "; ")
	if s.dropped > 0 {
		out += fmt.Sprintf("; and %d more", s.dropped)
	}
	return out
}

// No change plus a refusal fails: every completion shows a green check.
func (s *Server) finishTagScopeJob(changed int, reasons skipReasons, cancelled bool, noun, summary string) {
	if reasons.any() {
		summary += ": " + reasons.String()
	}
	var failed error
	if !cancelled && changed == 0 && reasons.any() {
		failed = errors.New(summary)
	}
	s.finishJob(failed, cancelled, fmt.Sprintf("%s cancelled (%s)", noun, summary), summary)
}

func (s *Server) resolveTagScope(r *http.Request) ([]int64, error) {
	q := r.Form
	// An empty ids is an empty selection; a missing one means the filter.
	if q.Has("ids") {
		idsStr := strings.TrimSpace(q.Get("ids"))
		if idsStr == "" {
			return nil, nil
		}
		var ids []int64
		for _, part := range strings.Split(idsStr, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("bad tag id %q", part)
			}
			ids = append(ids, id)
		}
		return ids, nil
	}
	// The listing's own filter, so the escalation acts on what the page shows.
	filter := s.tagListingFilter(tagListingParamsFrom(q))
	filter.PageIndex, filter.Limit = 0, 0
	return s.tagSvc().ListTagIDs(filter)
}

// On false the response is already written.
func (s *Server) startTagScopeJob(w http.ResponseWriter, r *http.Request) ([]int64, bool) {
	ids, err := s.resolveTagScope(r)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return nil, false
	}
	if !s.startJob(w, models.JobTypeTag) {
		return nil, false
	}
	return ids, true
}

func (s *Server) startTagScopeRun(w http.ResponseWriter, r *http.Request, run func(ids []int64)) {
	ids, ok := s.startTagScopeJob(w, r)
	if !ok {
		return
	}
	go run(ids)
	w.WriteHeader(http.StatusAccepted)
}

// op returns true for a changed row, false for a skip; an error is a refusal.
func (s *Server) runTagScopeLoop(ids []int64, gerund string, stride int, op func(id int64) (bool, error)) (changed, skipped int, reasons skipReasons, cancelled bool) {
	ctx := s.jobs.Context()
	total := len(ids)
	s.jobs.Update(0, total, gerund)
	for i, id := range ids {
		if ctx.Err() != nil {
			return changed, skipped, reasons, true
		}
		switch ok, err := op(id); {
		case err != nil:
			reasons.add(err)
			skipped++
		case ok:
			changed++
		default:
			skipped++
		}
		if (i+1)%stride == 0 || i+1 == total {
			s.jobs.Update(i+1, total, gerund)
		}
	}
	return changed, skipped, reasons, false
}

func skippedSuffix(summary string, skipped int) string {
	if skipped > 0 {
		summary += fmt.Sprintf(", skipped %d", skipped)
	}
	return summary
}

func (s *Server) batchTagCategoryPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	catID, err := strconv.ParseInt(r.FormValue("category_id"), 10, 64)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, "Invalid category.")
		return
	}
	merge := r.FormValue("merge") == "1"
	s.startTagScopeRun(w, r, func(ids []int64) { s.runBatchTagCategory(ids, catID, merge) })
}

func (s *Server) runBatchTagCategory(ids []int64, catID int64, merge bool) {
	mergedCount := 0
	changed, skipped, reasons, cancelled := s.runTagScopeLoop(ids, "moving tags…", 50, func(id int64) (bool, error) {
		var current int64
		if err := s.db().Read.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, id).Scan(&current); err == nil && current == catID {
			return false, nil
		}
		if !merge {
			if err := s.tagSvc().ChangeTagCategory(id, catID); err != nil {
				logx.Warnf("batch category tag %d: %v", id, err)
				return false, err
			}
			return true, nil
		}
		didMerge, err := s.tagSvc().ChangeTagCategoryMerge(id, catID)
		if err != nil {
			logx.Warnf("batch category tag %d: %v", id, err)
			return false, err
		}
		if didMerge {
			mergedCount++
		}
		return true, nil
	})

	s.active().InvalidateCaches()
	summary := fmt.Sprintf("moved %d tag(s)", changed-mergedCount)
	if mergedCount > 0 {
		summary += fmt.Sprintf(", merged %d", mergedCount)
	}
	s.finishTagScopeJob(changed, reasons, cancelled, "category move", skippedSuffix(summary, skipped))
}

func (s *Server) batchTagAliasPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	canonID, msg := s.resolveCanonicalTagInput(r.FormValue("canonical_id"), true)
	if msg != "" {
		flashStatus(w, http.StatusBadRequest, msg)
		return
	}
	s.startTagScopeRun(w, r, func(ids []int64) { s.runBatchTagAlias(ids, canonID) })
}

func (s *Server) runBatchTagAlias(ids []int64, canonID int64) {
	aliased, skipped, reasons, cancelled := s.runTagScopeLoop(ids, "aliasing tags…", 50, func(id int64) (bool, error) {
		if id == canonID {
			// The canonical is in the scope too; skip it without a refusal.
			return false, nil
		}
		if err := s.tagSvc().MergeTags(id, canonID); err != nil {
			logx.Warnf("batch alias tag %d: %v", id, err)
			return false, err
		}
		return true, nil
	})

	s.active().InvalidateCaches()
	summary := skippedSuffix(fmt.Sprintf("aliased %d tag(s) to %s", aliased, s.qualifiedTagName(canonID)), skipped)
	s.finishTagScopeJob(aliased, reasons, cancelled, "alias", summary)
}

func (s *Server) batchMergeFoldedPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.startTagScopeRun(w, r, s.runMergeFolded)
}

func (s *Server) runMergeFolded(ids []int64) {
	s.jobs.Update(0, len(ids), "merging folded tags…")
	res, err := s.tagSvc().MergeFolded(s.jobs.Context(), ids)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}
	s.active().InvalidateCaches()
	var reasons skipReasons
	for _, e := range res.Refused {
		reasons.add(e)
	}
	summary := skippedSuffix(fmt.Sprintf("merged %d folded tag(s)", res.Merged), res.Skipped)
	s.finishTagScopeJob(res.Merged, reasons, res.Cancelled, "folded merge", summary)
}

func (s *Server) batchTagImplyPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	remove := r.FormValue("mode") == "remove"
	targetID, msg := s.resolveCanonicalTagInput(r.FormValue("target"), !remove)
	if msg != "" {
		flashStatus(w, http.StatusBadRequest, msg)
		return
	}
	s.startTagScopeRun(w, r, func(ids []int64) { s.runBatchTagImply(ids, targetID, remove) })
}

func (s *Server) runBatchTagImply(ids []int64, targetID int64, remove bool) {
	ctx := s.jobs.Context()
	verb := "declaring implications…"
	if remove {
		verb = "removing implications…"
	}

	var removeClosure []int64
	if remove {
		var err error
		removeClosure, err = s.resolveRemoveClosure(targetID)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
	}

	changed, skipped, reasons, cancelled := s.runTagScopeLoop(ids, verb, 10, func(parentID int64) (bool, error) {
		if parentID == targetID {
			return false, nil
		}
		if remove {
			if err := s.tagSvc().RemoveImplication(parentID, targetID); err != nil {
				return false, err
			}
			if err := s.sweepImplicationRemovalInline(ctx, parentID, removeClosure); err != nil {
				logx.Warnf("batch imply sweep parent %d: %v", parentID, err)
			}
			return true, nil
		}
		created, err := s.tagSvc().AddImplicationFrom(parentID, targetID, "user")
		if err != nil {
			logx.Warnf("batch imply parent %d: %v", parentID, err)
			return false, err
		}
		if !created {
			return false, nil
		}
		if err := s.fanOutImplicationsInline(ctx, parentID); err != nil {
			logx.Warnf("batch imply fan-out parent %d: %v", parentID, err)
		}
		return true, nil
	})

	if err := s.tagSvc().RecalcIDs([]int64{targetID}); err != nil {
		logx.Warnf("batch imply recalc: %v", err)
	}
	s.active().InvalidateCaches()
	noun := "declared"
	if remove {
		noun = "removed"
	}
	summary := skippedSuffix(fmt.Sprintf("%s %d implication(s) of %s", noun, changed, s.qualifiedTagName(targetID)), skipped)
	s.finishTagScopeJob(changed, reasons, cancelled, "implication batch", summary)
}

// closure is the removed target's transitive closure, target included.
func (s *Server) sweepImplicationRemovalInline(ctx context.Context, parentID int64, closure []int64) error {
	return s.chunkImageTagsByParent(ctx, parentID, func(tx *sql.Tx, imageID int64) error {
		return propagateRemoveImplication(tx, imageID, closure)
	})
}
