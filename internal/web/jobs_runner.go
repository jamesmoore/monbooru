package web

import (
	"context"
	"errors"
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

// A cancel outranks the error: a cancelled worker returns
// context.Canceled, which is not a failure.
func (s *Server) settleJob(ctx context.Context, err error, cancelMsg, doneMsg string) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		s.jobs.Complete(cancelMsg)
		return nil
	}
	if err != nil {
		s.jobs.Fail(err.Error())
		return err
	}
	s.jobs.Complete(doneMsg)
	return nil
}

func (s *Server) finishJob(err error, cancelled bool, cancelMsg, doneMsg string) {
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}
	if cancelled {
		s.jobs.Complete(cancelMsg)
		return
	}
	s.jobs.Complete(doneMsg)
}
