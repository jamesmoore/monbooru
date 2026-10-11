package web

import (
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
	s.jobs.Finish(failed, cancelled, fmt.Sprintf("%s cancelled (%s)", noun, summary), summary)
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

func (s *Server) startTagScopeRun(w http.ResponseWriter, r *http.Request, run func(ids []int64)) {
	ids, err := s.resolveTagScope(r)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	s.startIDsJob(w, models.JobTypeTag, ids, run)
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
	canon, msg := s.resolveCanonicalTagInput(r.FormValue("canonical_id"), true)
	if msg != "" {
		flashStatus(w, http.StatusBadRequest, msg)
		return
	}
	s.startTagScopeRun(w, r, func(ids []int64) {
		canonID, err := s.canonicalID(canon)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.runBatchTagAlias(ids, canonID)
	})
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
	target, msg := s.resolveCanonicalTagInput(r.FormValue("target"), !remove)
	if msg != "" {
		flashStatus(w, http.StatusBadRequest, msg)
		return
	}
	s.startTagScopeRun(w, r, func(ids []int64) {
		targetID, err := s.canonicalID(target)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.runBatchTagImply(ids, targetID, remove)
	})
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
		removeClosure, err = s.tagSvc().ImplicationClosure(targetID)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
	}

	reapplied := 0
	changed, skipped, reasons, cancelled := s.runTagScopeLoop(ids, verb, 10, func(parentID int64) (bool, error) {
		if parentID == targetID {
			return false, nil
		}
		if remove {
			if err := s.tagSvc().RemoveImplication(parentID, targetID); err != nil {
				return false, err
			}
			if _, err := s.tagSvc().SweepImplicationRemoval(ctx, parentID, removeClosure, nil); err != nil {
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
			reapplied++
		}
		if _, err := s.tagSvc().PropagateImplication(ctx, parentID, nil); err != nil {
			logx.Warnf("batch imply fan-out parent %d: %v", parentID, err)
		}
		return true, nil
	})

	noun := "declared"
	if remove {
		noun = "removed"
	}
	summary := fmt.Sprintf("%s %d implication(s) of %s", noun, changed-reapplied, s.qualifiedTagName(targetID))
	if reapplied > 0 {
		summary += fmt.Sprintf(", re-applied %d", reapplied)
	}
	summary = skippedSuffix(summary, skipped)
	s.finishTagScopeJob(changed, reasons, cancelled, "implication batch", summary)
}
