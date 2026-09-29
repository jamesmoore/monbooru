package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/models"
)

// monloader's per-request cap for a batch lookup.
const ptrLookupChunk = 100

type lookupCandidate struct {
	id            int64
	sha256        string
	md5           string
	canonicalPath string
}

func lookupMD5(ctx context.Context, cx *galleryCtx, imageID int64, stored string) (string, error) {
	if stored != "" {
		return stored, nil
	}
	return gallery.ComputeAndStoreMD5(ctx, cx.DB, imageID)
}

const onlineLookupChunk = 100

func (s *Server) runPTRLookupPhase(ctx context.Context, cx *galleryCtx) (checked, matched int, err error) {
	cursor, cerr := s.ptrIndexCursor(ctx)
	if cerr != nil {
		return 0, 0, cerr
	}
	lookup.PTRCursor.Store(cursor)

	for {
		if ctx.Err() != nil {
			return checked, matched, nil
		}
		batch, berr := s.dueLookupCandidates(cx, lookup.BackendPTR, ptrLookupChunk)
		if berr != nil {
			return checked, matched, berr
		}
		if len(batch) == 0 {
			break
		}
		images := make([]ptrLookupImage, len(batch))
		for i, c := range batch {
			images[i] = ptrLookupImage{ImageID: c.id, SHA256: c.sha256}
		}
		results, answered, lerr := s.ptrBatchLookup(ctx, cx.Name, true, images)
		if lerr != nil {
			return checked, matched, lerr
		}
		if answered > 0 {
			cursor = answered
			lookup.PTRCursor.Store(cursor)
		}
		byHash := make(map[string]int64, len(batch))
		for _, c := range batch {
			byHash[c.sha256] = c.id
		}
		hits, recorded, aerr := applyPTRResults(cx, byHash, results, cursor)
		matched += hits
		checked += recorded
		if aerr != nil {
			return checked, matched, aerr
		}
		// Without an offset, a page nothing was recorded against comes
		// back identical.
		if recorded == 0 {
			break
		}
		s.jobs.Update(checked, checked+len(batch), fmt.Sprintf("[%s] PTR lookup…", cx.Name))
	}
	// Recorded outcomes move lookup: membership, which the cached match
	// ids predate.
	cx.InvalidateCaches()
	return checked, matched, nil
}

// A row whose apply fails stays unrecorded for the next pass, so
// recorded, not the batch length, tells a pager the page moved.
func applyPTRResults(cx *galleryCtx, byHash map[string]int64, results map[string][]string, cursor uint64) (matched, recorded int, err error) {
	for sha, id := range byHash {
		tags, hit := results[sha]
		result := lookup.ResultMiss
		if hit {
			if aerr := gallery.ApplyPTRTags(cx.DB, cx.TagSvc, id, tags); aerr != nil {
				logx.Warnf("ptr apply for image %d: %v", id, aerr)
				continue
			}
			result = lookup.ResultHit
			matched++
		}
		if rerr := lookup.Record(cx.DB, id, lookup.BackendPTR, result, cursor, time.Now()); rerr != nil {
			logx.Warnf("ptr record for image %d: %v", id, rerr)
			if err == nil {
				err = rerr
			}
			continue
		}
		recorded++
	}
	return matched, recorded, err
}

func (s *Server) ptrIndexCursor(ctx context.Context) (uint64, error) {
	resp, err := s.monloader().Do(ctx, http.MethodGet, "/api/v1/ptr/status", nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, peerStatusError{monloaderApp, resp.Status}
	}
	var out struct {
		Progress struct {
			UpdateIndex uint64 `json:"update_index"`
		} `json:"progress"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Progress.UpdateIndex, nil
}

// Rows drop out as they are recorded, so a plain LIMIT pages without an
// offset. Never-tried rows come first, then the oldest due: an inconclusive
// result is re-dated to now, so it sorts behind every older retry.
func (s *Server) dueLookupCandidates(cx *galleryCtx, backend string, limit int) ([]lookupCandidate, error) {
	due, args := lookup.DueClause(backend, time.Now())
	return db.QueryAll(cx.DB.Read, func(rows *sql.Rows) (lookupCandidate, error) {
		var c lookupCandidate
		err := rows.Scan(&c.id, &c.sha256, &c.md5, &c.canonicalPath)
		return c, err
	}, `SELECT i.id, i.sha256, i.md5, i.canonical_path
		 FROM images i
		 LEFT JOIN image_lookups l ON l.image_id = i.id AND l.backend = ?
		 WHERE `+lookup.CandidateClause(backend)+` AND `+due+`
		 ORDER BY l.next_due_at IS NOT NULL, l.next_due_at, i.id
		 LIMIT ?`,
		append(append([]any{backend}, args...), limit)...)
}

func (s *Server) dueLookupCount(cx *galleryCtx, backend string) int {
	due, args := lookup.DueClause(backend, time.Now())
	var n int
	if err := cx.DB.Read.QueryRow(
		`SELECT COUNT(*) FROM images i WHERE `+lookup.CandidateClause(backend)+` AND `+due, args...,
	).Scan(&n); err != nil {
		logx.Warnf("lookup due count %q: %v", cx.Name, err)
	}
	return n
}

type onlineLookupResult struct {
	queued       int
	skipped      int
	stillDue     int
	budgetSpent  bool
	resolved     int
	inconclusive int
}

// Reconciles first so the selection does not see rows stuck from a
// previous run.
func (s *Server) runOnlineLookupPhase(ctx context.Context, cx *galleryCtx) (onlineLookupResult, error) {
	var res onlineLookupResult
	res.resolved, res.inconclusive = s.reconcileLookups(ctx, cx)
	for ctx.Err() == nil {
		batch, err := s.dueLookupCandidates(cx, lookup.BackendBooru, onlineLookupChunk)
		if err != nil {
			return res, err
		}
		if len(batch) == 0 {
			break
		}
		queuedBefore := res.queued
		for _, c := range batch {
			if ctx.Err() != nil {
				break
			}
			md5, herr := lookupMD5(ctx, cx, c.id, c.md5)
			if herr != nil {
				// Skipped, never recorded as a miss: nothing was looked up.
				res.skipped++
				continue
			}
			jobID, eerr := s.enqueueHashLookup(ctx, c.id, cx.Name, lookup.BackendBooru, md5, c.sha256, true, true)
			switch {
			case errors.Is(eerr, errLookupBudgetSpent):
				res.budgetSpent = true
				res.stillDue = s.dueLookupCount(cx, lookup.BackendBooru)
				cx.InvalidateCaches()
				return res, nil
			case isPeerStatusErr(eerr):
				res.skipped++
				continue
			case eerr != nil:
				return res, eerr
			}
			s.recordLookupEnqueued(cx, c.id, lookup.BackendBooru, jobID)
			res.queued++
			s.jobs.Update(res.queued, res.queued+len(batch), fmt.Sprintf("[%s] online lookup…", cx.Name))
		}
		// Without an offset, a page nothing was enqueued from comes back
		// identical.
		if res.queued == queuedBefore {
			break
		}
	}
	res.stillDue = s.dueLookupCount(cx, lookup.BackendBooru)
	cx.InvalidateCaches()
	return res, nil
}

func (s *Server) scheduledOnlineLookup(cx *galleryCtx) error {
	ctx, err := s.startScheduledPhase(models.JobTypeLookup, "online lookup", cx.Name)
	if err != nil {
		return err
	}
	res, err := s.runOnlineLookupPhase(ctx, cx)
	if err != nil && ctx.Err() == nil {
		s.jobs.Fail(err.Error())
		return err
	}
	summary := "[" + cx.Name + "] " + onlinePhaseLine(res, ctx.Err() != nil)
	s.jobs.Complete(summary)
	if ctx.Err() != nil {
		return nil
	}
	s.sched.recordLookup(summary)
	if dropped := onlineDroppedLine(res); dropped != "" {
		s.sched.recordLookup("[" + cx.Name + "] " + dropped)
	}
	return nil
}

func onlinePhaseLine(res onlineLookupResult, cancelled bool) string {
	if cancelled {
		return fmt.Sprintf("online lookup cancelled (%d queued)", res.queued)
	}
	line := fmt.Sprintf("Online lookup: %d queued on monloader.", res.queued)
	if res.budgetSpent {
		line += fmt.Sprintf(" Daily budget reached; %d still due.", res.stillDue)
	} else if res.stillDue > 0 {
		line += fmt.Sprintf(" %d still due.", res.stillDue)
	}
	return line
}

func onlineDroppedLine(res onlineLookupResult) string {
	if res.resolved > 0 && res.inconclusive*2 > res.resolved {
		return fmt.Sprintf("Online lookup: monloader dropped %d of the %d lookups queued last run.", res.inconclusive, res.resolved)
	}
	return ""
}

func ptrPhaseLine(checked, matched int, err error, cancelled bool) string {
	switch {
	case errors.Is(err, errPTRBatchUnsupported):
		return "PTR lookup skipped: " + err.Error() + "."
	case cancelled:
		return fmt.Sprintf("PTR lookup cancelled (%d checked, %d matched)", checked, matched)
	case err != nil:
		return "PTR lookup failed: " + err.Error() + "."
	case checked == 0:
		return "PTR lookup: nothing is due."
	}
	return fmt.Sprintf("PTR lookup: %d checked, %d matched, %d no match.", checked, matched, checked-matched)
}

// Honours the ladder and the budget, so it cannot be leaned on to burn a
// day's quota.
func (s *Server) lookupDuePost(w http.ResponseWriter, r *http.Request) {
	cx := s.active()
	if cx == nil {
		writeInlineFlash(w, "err", "no active gallery")
		return
	}
	if !s.monloaderUsable() {
		writeInlineFlash(w, "err", "monloader is unreachable")
		return
	}
	s.cfgMu.RLock()
	sched := s.cfg.Schedule
	s.cfgMu.RUnlock()
	if !sched.LookupPTR && !sched.LookupBooru {
		writeInlineFlash(w, "err", "no lookup is enabled in Settings > Schedule")
		return
	}
	ptrOn := sched.RunsOn(config.ActionLookupPTR, cx.Name)
	booruOn := sched.RunsOn(config.ActionLookupBooru, cx.Name)
	if !ptrOn && !booruOn {
		writeInlineFlash(w, "err", fmt.Sprintf("lookups are off for gallery %q in Settings > Schedule", cx.Name))
		return
	}
	if !s.startJob(w, models.JobTypeLookup) {
		return
	}
	go func() {
		ctx := s.jobs.Context()
		var parts []string
		failed := false
		if ptrOn {
			if !s.monloaderPTRReady() {
				parts = append(parts, "PTR lookup skipped: the index is not ready on monloader.")
			} else {
				checked, matched, err := s.runPTRLookupPhase(ctx, cx)
				failed = err != nil && !errors.Is(err, errPTRBatchUnsupported) && ctx.Err() == nil
				parts = append(parts, ptrPhaseLine(checked, matched, err, ctx.Err() != nil))
			}
		}
		// A PTR failure ends that phase only.
		if booruOn && ctx.Err() == nil {
			if res, err := s.runOnlineLookupPhase(ctx, cx); err != nil && ctx.Err() == nil {
				failed = true
				parts = append(parts, "Online lookup failed: "+err.Error()+".")
			} else {
				parts = append(parts, onlinePhaseLine(res, ctx.Err() != nil))
				if dropped := onlineDroppedLine(res); dropped != "" {
					parts = append(parts, dropped)
				}
			}
		}
		if failed {
			s.jobs.Fail(strings.Join(parts, " "))
			return
		}
		s.jobs.Complete(strings.Join(parts, " "))
	}()
	writeInlineFlash(w, "ok", "Lookup started. Watch the status bar.")
}

func (s *Server) scheduledPTRLookup(cx *galleryCtx) error {
	ctx, err := s.startScheduledPhase(models.JobTypeLookup, "ptr lookup", cx.Name)
	if err != nil {
		return err
	}
	checked, matched, err := s.runPTRLookupPhase(ctx, cx)
	summary := "[" + cx.Name + "] " + ptrPhaseLine(checked, matched, err, ctx.Err() != nil)
	switch {
	case errors.Is(err, errPTRBatchUnsupported), ctx.Err() != nil:
		s.jobs.Complete(summary)
		return nil
	case err != nil:
		s.jobs.Fail(err.Error())
		return err
	}
	s.jobs.Complete(summary)
	s.sched.recordLookup(summary)
	return nil
}
