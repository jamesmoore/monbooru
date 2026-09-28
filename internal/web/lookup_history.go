package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/lookup"
)

// Comfortably past a real backlog, so a busy queue is never mistaken for
// a lost callback.
const lookupGrace = 6 * time.Hour

func lookupBackendsFor(backend string) []string {
	if backend == "all" {
		return []string{lookup.BackendPTR, lookup.BackendBooru}
	}
	return []string{backend}
}

// Only logs a failure: the job is already queued on monloader.
func (s *Server) recordLookupEnqueued(cx *galleryCtx, imageID int64, backend string, jobID int64) {
	if err := lookup.Enqueued(cx.DB, imageID, lookupBackendsFor(backend), jobID, time.Now()); err != nil {
		logx.Warnf("lookup history for image %d: %v", imageID, err)
	}
}

type lookupView struct {
	ImageID int64
	// Candidate excludes an archive: no booru or repository indexes its
	// own hash, only its pages'.
	Candidate bool
	Backends  []lookupBackendView
}

type lookupBackendView struct {
	Backend string
	Label   string
	// Enabled is the image's own opt-in; ScheduleOn is the phase in Settings.
	Enabled    bool
	ScheduleOn bool
	GalleryOff bool
	Exhausted  bool
	Attempts   int
	QueuedAt   time.Time
	LastAt     time.Time
	LastResult string
	NextDueAt  time.Time
}

func (v lookupBackendView) Tried() bool { return !v.LastAt.IsZero() || !v.QueuedAt.IsZero() }

type lookupStatus struct {
	Text string
	Tone string
}

// Case order matters: an in-flight attempt outranks a spent ladder, and both
// outrank the due date.
func (v lookupBackendView) Status() lookupStatus {
	switch {
	case !v.ScheduleOn:
		return lookupStatus{Text: "schedule off"}
	case v.GalleryOff:
		return lookupStatus{Text: "off for this gallery"}
	case !v.Enabled:
		return lookupStatus{Text: "not scheduled"}
	case !v.QueuedAt.IsZero():
		return lookupStatus{Text: "queued", Tone: "queued"}
	case v.Exhausted:
		return lookupStatus{Text: "nothing found", Tone: "verdict"}
	case v.LastResult == lookup.ResultHit:
		return lookupStatus{Text: "matched " + localDay(v.LastAt), Tone: "hit"}
	case v.NextDueAt.After(time.Now()):
		return lookupStatus{Text: "due " + localDay(v.NextDueAt)}
	}
	// Not "due tonight": a budget-bound run reaches it only once the
	// backlog ahead of it clears.
	return lookupStatus{Text: "due now"}
}

func (s *Server) lookupViewFor(cx *galleryCtx, imageID int64) lookupView {
	v := lookupView{ImageID: imageID}
	s.cfgMu.RLock()
	sched := s.cfg.Schedule
	s.cfgMu.RUnlock()

	ptrOn, booruOn := true, true
	var sourced int
	var fileType string
	if err := cx.DB.Read.QueryRow(
		`SELECT i.scheduled_lookup_ptr, i.scheduled_lookup, i.file_type,
		        EXISTS (SELECT 1 FROM image_sources s WHERE s.image_id = i.id AND s.url <> '')
		 FROM images i WHERE i.id = ?`, imageID,
	).Scan(&ptrOn, &booruOn, &fileType, &sourced); err != nil {
		logx.Warnf("lookup view for image %d: %v", imageID, err)
		return v
	}
	v.Candidate = sourced == 0 && fileType != "cbz"

	rows, err := lookup.ForImage(cx.DB, imageID)
	if err != nil {
		logx.Warnf("lookup view for image %d: %v", imageID, err)
		return v
	}
	for _, b := range []struct {
		backend, label, action string
		optedIn                bool
	}{
		{lookup.BackendPTR, "PTR", config.ActionLookupPTR, ptrOn},
		{lookup.BackendBooru, "boorus", config.ActionLookupBooru, booruOn},
	} {
		r := rows[b.backend]
		on := sched.Enabled(b.action)
		v.Backends = append(v.Backends, lookupBackendView{
			Backend: b.backend, Label: b.label, Enabled: b.optedIn,
			ScheduleOn: on, GalleryOff: on && !sched.RunsOn(b.action, cx.Name),
			Exhausted: r.Exhausted(), Attempts: r.Attempts, QueuedAt: r.QueuedAt,
			LastAt: r.LastAt, LastResult: r.LastResult, NextDueAt: r.NextDueAt,
		})
	}
	return v
}

func scheduledLookupBackend(r *http.Request) string {
	if r.FormValue("backend") == lookup.BackendPTR {
		return lookup.BackendPTR
	}
	return lookup.BackendBooru
}

func (s *Server) scheduledLookupPost(w http.ResponseWriter, r *http.Request) {
	id, cx, _, ok := s.imageAndGallery(w, r)
	if !ok {
		return
	}
	backend := scheduledLookupBackend(r)
	on := 0
	if r.FormValue("on") == "1" {
		on = 1
	}
	if _, err := cx.DB.Write.Exec(
		`UPDATE images SET `+lookup.FlagColumn(backend)+` = ? WHERE id = ?`, on, id); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	// Opting back in is [look again]: a spent ladder left behind would
	// mean nothing happens.
	if on == 1 {
		if err := lookup.Reset(cx.DB, id, backend, time.Now()); err != nil {
			logx.Warnf("lookup reset for image %d: %v", id, err)
		}
	}
	// The lookup: filters read this flag and the ladder, which the cached
	// match ids predate.
	cx.InvalidateCaches()
	s.renderScheduledLookup(w, r, cx, id)
}

func (s *Server) scheduledLookupResetPost(w http.ResponseWriter, r *http.Request) {
	id, cx, _, ok := s.imageAndGallery(w, r)
	if !ok {
		return
	}
	if err := lookup.Reset(cx.DB, id, scheduledLookupBackend(r), time.Now()); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	cx.InvalidateCaches()
	s.renderScheduledLookup(w, r, cx, id)
}

func (s *Server) renderScheduledLookup(w http.ResponseWriter, r *http.Request, cx *galleryCtx, id int64) {
	view := s.lookupViewFor(cx, id)
	csrf := s.csrfToken(sessionFromContext(r.Context()))
	s.renderTemplate(w, "partials/scheduled_lookup.html", map[string]any{
		"Lookup": view, "CSRFToken": csrf,
	})
	// The history, in the other column, carries [look again], so it
	// re-renders with the ladder.
	s.renderTemplate(w, "partials/lookup_history.html", map[string]any{
		"Lookup": view, "CSRFToken": csrf, "OOB": true,
	})
}

// Never from a render path: a monloader round trip there would spend the
// page's latency budget on a link that may be down.
func (s *Server) reconcileLookups(ctx context.Context, cx *galleryCtx) (resolved, inconclusive int) {
	waiting, err := lookup.Waiting(cx.DB, time.Now().Add(-lookupGrace))
	if err != nil {
		logx.Warnf("lookup reconcile %q: %v", cx.Name, err)
		return 0, 0
	}
	for _, f := range waiting {
		if ctx.Err() != nil {
			return resolved, inconclusive
		}
		result, ok := s.lookupJobOutcome(ctx, f.JobID)
		if !ok {
			continue
		}
		if result == "" {
			// Evidence about the plumbing, not the image: the ladder
			// stays and the row is due now.
			inconclusive++
			result = lookup.ResultError
		}
		if err := lookup.Record(cx.DB, f.ImageID, f.Backend, result, 0, time.Now()); err != nil {
			logx.Warnf("lookup reconcile %q image %d: %v", cx.Name, f.ImageID, err)
			continue
		}
		resolved++
	}
	return resolved, inconclusive
}

// false leaves the row in flight. An empty result is inconclusive: moving
// the ladder on it would walk an image to "nothing found" while monloader
// is down.
func (s *Server) lookupJobOutcome(ctx context.Context, jobID int64) (string, bool) {
	if jobID == 0 {
		return "", true // a monloader that reports no job id
	}
	resp, err := s.monloader().Do(ctx, http.MethodGet, "/api/v1/queue/"+strconv.FormatInt(jobID, 10), nil)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		// Aged out of the finished ring, or never existed.
		return "", true
	}
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var job struct {
		Status string `json:"status"`
		Items  []struct {
			Outcome   string `json:"outcome"`
			ErrorCode string `json:"error_code"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return "", false
	}
	switch job.Status {
	case "queued", "running":
		return "", false
	case "canceled", "interrupted":
		return "", true
	case "failed":
		return lookup.ResultError, true
	}
	for _, it := range job.Items {
		if it.Outcome == "enriched" {
			return lookup.ResultHit, true
		}
		if it.ErrorCode == "hash_not_found" {
			return lookup.ResultMiss, true
		}
	}
	return lookup.ResultError, true
}

func (s *Server) reconcileAllLookups() {
	if !s.monloaderUsable() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctxs := s.allContexts()
	for _, cx := range ctxs {
		s.reconcileLookups(ctx, cx)
	}
}
