package web

import (
	"net/http"

	"github.com/monbooru/monbooru/internal/models"
)

func (s *Server) jobDismissPost(w http.ResponseWriter, r *http.Request) {
	s.jobs.Dismiss()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) jobCancelPost(w http.ResponseWriter, r *http.Request) {
	s.jobs.Cancel()
	w.WriteHeader(http.StatusNoContent)
}

type jobStatusView struct {
	Job       *models.JobState
	Downloads []*activeDownload
}

func (s *Server) jobStatusHandler(w http.ResponseWriter, r *http.Request) {
	// MarkViewed first: a finished job's first render starts its dismiss timer.
	s.jobs.MarkViewed()
	s.renderTemplate(w, "partials/job_status.html", jobStatusView{Job: s.jobs.Get(), Downloads: s.downloads.snapshot()})
}

func (s *Server) syncTrigger(w http.ResponseWriter, r *http.Request) {
	if cx := s.active(); cx == nil || cx.Degraded {
		flashStatus(w, http.StatusServiceUnavailable, "Sync unavailable: gallery path is unreadable.")
		return
	}
	if !s.startJob(w, models.JobTypeSync) {
		return
	}
	// Resolved under the request's RLock; the gallery mutations refuse
	// while a job runs, so cx stays open for the sync.
	cx := s.active()
	maxFileSizeMB := s.maxFileSizeMB()
	go func() {
		ctx := s.jobs.Context()
		result, err := cx.Sync(ctx, maxFileSizeMB, s.ingestNaming(cx.Name), s.jobs.Update)
		_ = s.settleJob(ctx, err, "sync cancelled", result.Summary())
	}()

	redirectTo := sameOriginReferer(r)
	if isHTMXRequest(r) {
		w.Header().Set("HX-Trigger", "syncStarted")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	http.Redirect(w, r, redirectTo, http.StatusSeeOther)
}
