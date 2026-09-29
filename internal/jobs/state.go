// Package jobs runs one background job at a time: two jobs writing the
// same SQLite file would only contend.
package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/models"
)

var ErrJobRunning = errors.New("a job is already running")

const (
	dismissDelay       = 30 * time.Second
	viewedDismissDelay = 6 * time.Second
)

type Manager struct {
	mu     sync.Mutex
	state  *models.JobState
	timer  *time.Timer
	ctx    context.Context
	cancel context.CancelFunc
	// Blocks Start where no job is running, such as between a scheduled
	// run's phases.
	scheduleHeld bool
	viewed       bool
	finished     uint64
}

func NewManager() *Manager { return &Manager{} }

func (m *Manager) clearStateLocked() {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	m.ctx, m.cancel = nil, nil
	m.state = nil
	m.viewed = false
}

func (m *Manager) Start(jobType string, galleries ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scheduleHeld {
		return ErrJobRunning
	}
	return m.startLocked(jobType, galleries)
}

// StartScheduled skips the reservation check; only the holder of
// BeginSchedule may call it.
func (m *Manager) StartScheduled(jobType string, galleries ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked(jobType, galleries)
}

func (m *Manager) startLocked(jobType string, galleries []string) error {
	if m.state != nil && m.state.Running {
		return ErrJobRunning
	}
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	m.viewed = false
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.state = &models.JobState{
		Running:   true,
		JobType:   jobType,
		Galleries: galleries,
		StartedAt: time.Now().UTC(),
		Message:   "Starting...",
	}
	return nil
}

// BeginSchedule makes Start refuse until EndSchedule, which the caller
// must defer.
func (m *Manager) BeginSchedule() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scheduleHeld {
		return ErrJobRunning
	}
	if m.state != nil && m.state.Running {
		return ErrJobRunning
	}
	m.scheduleHeld = true
	return nil
}

func (m *Manager) EndSchedule() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scheduleHeld = false
}

func (m *Manager) IsScheduleHeld() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scheduleHeld
}

func (m *Manager) Context() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil {
		return context.Background()
	}
	return m.ctx
}

// Cancel also drops the schedule reservation, so a scheduled run stops at
// its next phase.
func (m *Manager) Cancel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	m.scheduleHeld = false
}

func (m *Manager) Update(processed, total int, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == nil {
		return
	}
	m.state.Processed = processed
	m.state.Total = total
	m.state.Message = message
}

func (m *Manager) Complete(summary string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == nil {
		return
	}
	now := time.Now().UTC()
	m.state.Running = false
	m.state.FinishedAt = &now
	m.state.Summary = summary
	m.state.Message = ""
	m.finished++
	m.scheduleAutoDismiss()
}

func (m *Manager) Fail(errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == nil {
		return
	}
	now := time.Now().UTC()
	m.state.Running = false
	m.state.FinishedAt = &now
	m.state.Error = errMsg
	m.finished++
	m.scheduleAutoDismiss()
}

func (m *Manager) Get() *models.JobState {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == nil {
		return nil
	}
	copy := *m.state
	return &copy
}

// IsRunning is also true while a schedule reservation is held.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scheduleHeld {
		return true
	}
	return m.state != nil && m.state.Running
}

// Finished only grows, so a watcher sampling less often than the
// auto-dismiss still sees a job end.
func (m *Manager) Finished() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.finished
}

// MarkViewed shortens the auto-dismiss, except after a failure: any open
// tab's poll counts as a view, and the status bar is the only report the
// error gets.
func (m *Manager) MarkViewed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil || m.state.Running || m.viewed || m.state.Error != "" {
		return
	}
	m.viewed = true
	m.armDismiss(viewedDismissDelay)
}

func (m *Manager) Dismiss() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil || m.state.Running {
		return
	}
	m.clearStateLocked()
}

func (m *Manager) SetWatcherMessage(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != nil && m.state.Running {
		m.state.WatcherNotices++
		return
	}
	now := time.Now().UTC()
	m.state = &models.JobState{
		Running:    false,
		JobType:    models.JobTypeWatcher,
		Summary:    msg,
		FinishedAt: &now,
	}
	m.scheduleAutoDismiss()
}

// Caller must hold m.mu.
func (m *Manager) scheduleAutoDismiss() {
	m.armDismiss(dismissDelay)
}

// Caller must hold m.mu.
func (m *Manager) armDismiss(d time.Duration) {
	if m.timer != nil {
		m.timer.Stop()
	}
	m.timer = time.AfterFunc(d, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.state == nil || m.state.Running {
			return
		}
		m.clearStateLocked()
	})
}
