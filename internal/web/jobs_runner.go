package web

import (
	"context"
	"net/http"
)

// others are galleries the job writes besides the active one.
func (s *Server) startJob(w http.ResponseWriter, jobType string, others ...string) bool {
	if err := s.jobs.Start(jobType, append([]string{s.activeGallery()}, others...)...); err != nil {
		flashStatus(w, http.StatusConflict, "A job is already running.")
		return false
	}
	return true
}

// goJob answers with the started flash once work runs on the job context.
func (s *Server) goJob(w http.ResponseWriter, jobType, started string, work func(ctx context.Context)) {
	if !s.startJob(w, jobType) {
		return
	}
	go work(s.jobs.Context())
	writeInlineFlash(w, "ok", started)
}

// No ids answers 202 with nothing started.
func (s *Server) startIDsJob(w http.ResponseWriter, jobType string, ids []int64, run func([]int64), others ...string) {
	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !s.startJob(w, jobType, others...) {
		return
	}
	go run(ids)
	w.WriteHeader(http.StatusAccepted)
}
