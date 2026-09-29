package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/tagger"
)

// Long enough that a cold start does not compete with the operator's
// first pages.
const scheduleGraceDelay = 5 * time.Minute

// Past its slot by more than this, the clock was asleep or stepped: the
// slot goes to the catch-up rule instead of running late.
const scheduleWakeSlack = 2 * time.Minute

type scheduler struct {
	// Buffered to 1 with non-blocking sends, so concurrent saves coalesce
	// into one wake-up.
	reload chan struct{}

	mu            sync.Mutex
	lastRun       time.Time
	lastDur       time.Duration
	lastInfo      string
	lookupInfo    []string
	galleryOffset int
	catchUpDay    time.Time
}

func newScheduler() *scheduler {
	return &scheduler{reload: make(chan struct{}, 1)}
}

func (sc *scheduler) requestReload() {
	select {
	case sc.reload <- struct{}{}:
	default:
	}
}

func (sc *scheduler) markCatchUpDay(at time.Time) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.catchUpDay = at
}

func (sc *scheduler) catchUpFiredToday(now time.Time) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return !sc.catchUpDay.IsZero() && !localDayStart(now).After(localDayStart(sc.catchUpDay))
}

func (sc *scheduler) nextOffset(n int) int {
	if n <= 1 {
		return 0
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	start := sc.galleryOffset % n
	sc.galleryOffset = (start + 1) % n
	return start
}

func (sc *scheduler) recordRun(started time.Time, dur time.Duration, info string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.lastRun, sc.lastDur, sc.lastInfo = started, dur, info
}

func (sc *scheduler) recordLookup(summary string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.lookupInfo = append(sc.lookupInfo, summary)
}

func (sc *scheduler) clearLookup() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.lookupInfo = nil
}

func (sc *scheduler) lastRunStatus() ScheduleStatus {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return ScheduleStatus{
		LastRun: sc.lastRun, LastDur: sc.lastDur, LastInfo: sc.lastInfo,
		LookupInfo: append([]string(nil), sc.lookupInfo...),
	}
}

func (s *Server) runScheduler() {
	// Its own goroutine: the grace delay must not hold the clock trigger
	// unarmed, or a start shortly before schedule.time misses that day.
	go s.scheduleCatchUp()
	for {
		next, ok := s.nextScheduledFire(time.Now())
		if !ok {
			select {
			case <-s.done:
				return
			case <-s.sched.reload:
				continue
			case <-time.After(time.Hour):
				continue
			}
		}
		logx.Infof("scheduler: next run at %s (in %s)", next.Format(time.RFC3339), max(time.Until(next), 0).Round(time.Second))
	slot:
		for {
			wait, fire, missed := clockWake(next, time.Now())
			switch {
			case missed:
				logx.Infof("scheduler: the %s run went by while this machine was asleep", next.Format(time.RFC3339))
				go s.scheduleCatchUp()
				break slot
			case fire:
				if s.clockFireOwed(time.Now()) {
					s.runScheduledActions()
				} else {
					logx.Infof("scheduler: today's pass already ran; skipping the clock trigger")
				}
				break slot
			}
			select {
			case <-s.done:
				return
			case <-s.sched.reload:
				break slot
			case <-time.After(wait):
			}
		}
	}
}

// Timers run on a clock that stops in suspend, so the wall clock is
// re-read at least every minute.
func clockWake(next, now time.Time) (wait time.Duration, fire, missed bool) {
	if now.Before(next) {
		return min(next.Sub(now), time.Minute), false, false
	}
	if now.Sub(next) > scheduleWakeSlack {
		return 0, false, true
	}
	return 0, true, false
}

func (s *Server) scheduleCatchUp() {
	if !s.catchUpOwed() {
		return
	}
	select {
	case <-s.done:
		return
	case <-time.After(scheduleGraceDelay):
	}
	// Re-read: the schedule may have changed during the grace delay.
	if !s.catchUpOwed() {
		return
	}
	logx.Infof("scheduler: running the pass this machine was not awake for")
	s.sched.markCatchUpDay(time.Now())
	s.runScheduledActions()
}

func (s *Server) catchUpOwed() bool {
	sched, galleries := s.scheduleView()
	return shouldCatchUp(sched, galleries, s.lastScheduledRun(), time.Now())
}

// Unpaired, the lookup phases are off in this copy only; the saved
// switches wait for the next pairing.
func (s *Server) scheduleView() (config.ScheduleConfig, []string) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	sched := s.cfg.Schedule
	if s.cfg.FindPairedToken(monloaderApp) == nil {
		sched.LookupPTR, sched.LookupBooru = false, false
	}
	names := make([]string, len(s.cfg.Galleries))
	for i, g := range s.cfg.Galleries {
		names[i] = g.Name
	}
	return sched, names
}

// The clock skips only a day the startup catch-up covered, or a boot
// before schedule.time would run twice; a manual run does not spend the
// night's pass.
func (s *Server) clockFireOwed(now time.Time) bool {
	s.cfgMu.RLock()
	mode := s.cfg.Schedule.EffectiveMode()
	s.cfgMu.RUnlock()
	if mode != config.ScheduleAtTimeCatchup || !s.sched.catchUpFiredToday(now) {
		return true
	}
	// A catch-up the job lane refused recorded no run, so the clock still
	// owes the day.
	return dayPassOwed(s.lastScheduledRun(), now)
}

// on_start has no clock trigger, so this check is its only once-per-day
// bound. at_time_catchup owes the most recent slot, so a morning boot
// after an evening slot that ran owes nothing.
func shouldCatchUp(sched config.ScheduleConfig, galleries []string, last, now time.Time) bool {
	switch sched.EffectiveMode() {
	case config.ScheduleAtTimeCatchup, config.ScheduleOnStart:
	default:
		return false
	}
	if !schedHasWork(sched, galleries) {
		return false
	}
	if t, err := parseScheduleTime(sched.Time); err == nil && sched.EffectiveMode() == config.ScheduleAtTimeCatchup {
		return last.Before(mostRecentSlot(t, now))
	}
	return dayPassOwed(last, now)
}

func mostRecentSlot(t schedTime, now time.Time) time.Time {
	local := now.In(time.Local)
	year, month, day := local.Date()
	slot := time.Date(year, month, day, t.hour, t.minute, 0, 0, time.Local)
	if slot.After(local) {
		slot = time.Date(year, month, day-1, t.hour, t.minute, 0, 0, time.Local)
	}
	return slot
}

// The local calendar day, not 24 hours: a boot slightly earlier than
// yesterday's run would otherwise skip a day.
func dayPassOwed(last, now time.Time) bool {
	return last.IsZero() || localDayStart(now).After(localDayStart(last))
}

func localDayStart(t time.Time) time.Time {
	local := t.In(time.Local)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
}

func scheduleFiresAtTime(sched config.ScheduleConfig, galleries []string) bool {
	if !schedHasWork(sched, galleries) {
		return false
	}
	mode := sched.EffectiveMode()
	return mode == config.ScheduleAtTime || mode == config.ScheduleAtTimeCatchup
}

func (s *Server) nextScheduledFire(now time.Time) (time.Time, bool) {
	sched, galleries := s.scheduleView()
	if !scheduleFiresAtTime(sched, galleries) {
		return time.Time{}, false
	}
	t, err := parseScheduleTime(sched.Time)
	if err != nil {
		return time.Time{}, false
	}
	year, month, day := now.Date()
	fire := time.Date(year, month, day, t.hour, t.minute, 0, 0, now.Location())
	// A day the catch-up covered is skipped too, so its time is not the
	// next fire.
	if !fire.After(now) || !s.clockFireOwed(now) {
		// day+1, not Add(24h), which slips the local time by an hour
		// across DST.
		fire = time.Date(year, month, day+1, t.hour, t.minute, 0, 0, now.Location())
	}
	return fire, true
}

type schedTime struct{ hour, minute int }

func parseScheduleTime(v string) (schedTime, error) {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 {
		return schedTime{}, fmt.Errorf("bad time %q", v)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return schedTime{}, fmt.Errorf("bad hour in %q", v)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return schedTime{}, fmt.Errorf("bad minute in %q", v)
	}
	return schedTime{hour: h, minute: m}, nil
}

func schedHasAnyEnabled(sc config.ScheduleConfig) bool {
	return sc.SyncGallery || sc.RemoveOrphans || sc.RunAutoTaggers || sc.FindRelationPairs ||
		sc.LookupPTR || sc.LookupBooru
}

func schedHasWork(sc config.ScheduleConfig, galleries []string) bool {
	for _, action := range config.ScheduleActions {
		for _, g := range galleries {
			if sc.RunsOn(action, g) {
				return true
			}
		}
	}
	return false
}

// The lookup phases run last: an external limit can cut them short without
// costing the rest, and their writes must not interleave with autotag's.
func (s *Server) runScheduledActions() {
	if err := s.jobs.BeginSchedule(); err != nil {
		logx.Warnf("scheduler: skipping run (a job is already running)")
		return
	}
	defer s.jobs.EndSchedule()

	started := time.Now()
	s.sched.clearLookup()
	var failures []string
	cancelled := false
	defer func() {
		info := "OK"
		switch {
		case len(failures) > 0:
			info = strings.Join(failures, "; ")
		case cancelled:
			info = "cancelled"
		}
		s.recordScheduleRun(started, time.Since(started), info)
	}()

	sched, _ := s.scheduleView()

	st := s.galleryState()
	names := make([]string, 0, len(st.contexts))
	for name := range st.contexts {
		names = append(names, name)
	}
	// Sorted, then rotated run to run: a budget too small for the first
	// gallery would otherwise starve the rest.
	slices.Sort(names)
	names = rotateStrings(names, s.sched.nextOffset(len(names)))

	// A cancel drops the reservation; without this check one click would
	// stop only the current phase.
	abort := func() bool {
		if !s.jobs.IsScheduleHeld() {
			logx.Infof("scheduler: run cancelled mid-flight; remaining phases skipped")
			cancelled = true
			return true
		}
		return false
	}

	for _, name := range names {
		if abort() {
			return
		}
		cx := s.get(name)
		runs := func(action string) bool { return sched.RunsOn(action, name) }
		if cx == nil || !slices.ContainsFunc(config.ScheduleActions, runs) {
			continue
		}
		logx.Infof("scheduler: running actions on gallery %q", name)

		if runs(config.ActionSyncGallery) && !cx.Degraded {
			if err := s.scheduledSync(cx); err != nil {
				failures = append(failures, "sync "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
		if runs(config.ActionRemoveOrphans) {
			if err := s.scheduledRemoveOrphans(cx); err != nil {
				failures = append(failures, "remove-orphans "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
		if runs(config.ActionRunAutoTaggers) && tagger.IsAvailable(s.cfgSnapshot()) {
			if err := s.scheduledAutotag(cx); err != nil {
				failures = append(failures, "autotag "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
		if runs(config.ActionFindRelationPairs) {
			if err := s.scheduledFindRelationPairs(cx); err != nil {
				failures = append(failures, "find-pairs "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
		if runs(config.ActionLookupPTR) {
			switch {
			case !s.monloaderUsable():
				s.sched.recordLookup("[" + name + "] Lookup skipped: monloader unreachable.")
			case !s.monloaderPTRReady():
				s.sched.recordLookup("[" + name + "] PTR lookup skipped: monloader's index is not ready.")
			default:
				if err := s.scheduledPTRLookup(cx); err != nil {
					failures = append(failures, "ptr-lookup "+name+": "+err.Error())
				}
			}
			if abort() {
				return
			}
		}
		if runs(config.ActionLookupBooru) {
			if !s.monloaderUsable() {
				s.sched.recordLookup("[" + name + "] Online lookup skipped: monloader unreachable.")
			} else if err := s.scheduledOnlineLookup(cx); err != nil {
				failures = append(failures, "online-lookup "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
	}
}

func (s *Server) monloaderPTRReady() bool {
	ready := s.mlStatus.Seed().PTR
	return ready
}

func (s *Server) startScheduledPhase(jobType, phase, gallery string) (context.Context, error) {
	if err := s.jobs.StartScheduled(jobType, gallery); err != nil {
		logx.Warnf("scheduler %s %q: %v", phase, gallery, err)
		return nil, err
	}
	return s.jobs.Context(), nil
}

func rotateStrings(names []string, offset int) []string {
	if offset <= 0 || offset >= len(names) {
		return names
	}
	return append(append([]string(nil), names[offset:]...), names[:offset]...)
}

func (s *Server) scheduledFindRelationPairs(cx *galleryCtx) error {
	ctx, err := s.startScheduledPhase(models.JobTypeRelations, "find-pairs", cx.Name)
	if err != nil {
		return err
	}
	s.cfgMu.RLock()
	tagPairs := s.cfg.Relations.TagPairs
	tagPairThreshold := s.cfg.Relations.TagPairThreshold
	s.cfgMu.RUnlock()
	opts := relations.FindPairsOptions{
		Distance:         int(relations.IncrementalProbeDistance.Load()),
		Replace:          false,
		ThumbnailsPath:   cx.ThumbnailsPath,
		TagPairs:         tagPairs,
		TagPairThreshold: config.ClampTagPairThreshold(tagPairThreshold),
	}
	added, err := relations.FindPairs(ctx, cx.DB, cx.BKTree, opts, s.jobs.Update)
	return s.settleJob(ctx, err,
		fmt.Sprintf("[%s] find-pairs cancelled (%d added)", cx.Name, added),
		fmt.Sprintf("[%s] find-pairs added %d candidate(s).", cx.Name, added))
}

func (s *Server) scheduledSync(cx *galleryCtx) error {
	ctx, err := s.startScheduledPhase(models.JobTypeSync, "sync", cx.Name)
	if err != nil {
		return err
	}
	result, err := cx.Sync(ctx, s.maxFileSizeMB(), s.ingestNaming(cx.Name), s.jobs.Update)
	if err := s.settleJob(ctx, err,
		fmt.Sprintf("[%s] sync cancelled (%s)", cx.Name, result.Summary()),
		fmt.Sprintf("[%s] %s", cx.Name, result.Summary())); err != nil {
		logx.Warnf("scheduler sync %q: %v", cx.Name, err)
		return err
	}
	return nil
}

func (s *Server) scheduledRemoveOrphans(cx *galleryCtx) error {
	ctx, err := s.startScheduledPhase(models.JobTypePruneThumbs, "orphans", cx.Name)
	if err != nil {
		return err
	}
	removed, processed, total, err := s.runOrphanSweep(ctx, cx)
	s.finishJob(err, ctx.Err() != nil,
		fmt.Sprintf("[%s] orphan sweep cancelled (%d/%d scanned, %d removed)", cx.Name, processed, total, removed),
		fmt.Sprintf("[%s] removed %d orphaned thumbnail(s)", cx.Name, removed))
	if err != nil {
		logx.Warnf("scheduler orphans %q: %v", cx.Name, err)
		return err
	}
	logx.Infof("scheduler: [%s] removed %d orphaned thumbnail(s)", cx.Name, removed)
	return nil
}

// A failed id read must fail the sweep: a partial set would make live
// thumbnails look orphaned.
func (s *Server) runOrphanSweep(ctx context.Context, cx *galleryCtx) (removed, processed, total int, err error) {
	entries, err := os.ReadDir(cx.ThumbnailsPath)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read thumbnails dir: %w", err)
	}
	total = len(entries)
	ids, err := db.QueryIDsContext(ctx, cx.DB.Read, `SELECT id FROM images`)
	if err != nil {
		return 0, 0, total, err
	}
	known := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		known[id] = struct{}{}
	}

	s.jobs.Update(0, total, fmt.Sprintf("[%s] pruning…", cx.Name))
	for i, e := range entries {
		if ctx.Err() != nil {
			return removed, processed, total, nil
		}
		if e.IsDir() {
			continue
		}
		processed++
		name := e.Name()
		var idStr string
		switch {
		case strings.HasSuffix(name, "_hover.webp"):
			idStr = strings.TrimSuffix(name, "_hover.webp")
		case strings.HasSuffix(name, "_view.jpg"):
			idStr = strings.TrimSuffix(name, "_view.jpg")
		case strings.HasSuffix(name, ".jpg"):
			idStr = strings.TrimSuffix(name, ".jpg")
		default:
			continue
		}
		id, parseErr := strconv.ParseInt(idStr, 10, 64)
		if parseErr != nil {
			continue
		}
		if _, ok := known[id]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(cx.ThumbnailsPath, name)); err == nil {
			removed++
		}
		if (i+1)%50 == 0 || i == total-1 {
			s.jobs.Update(i+1, total, fmt.Sprintf("[%s] pruning…", cx.Name))
		}
	}
	return removed, processed, total, nil
}

func (s *Server) scheduledAutotag(cx *galleryCtx) error {
	ids, err := db.QueryIDs(cx.DB.Read,
		`SELECT i.id FROM images i WHERE i.is_missing = 0
		 AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.is_auto = 1)`,
	)
	if err != nil {
		logx.Warnf("scheduler autotag %q: %v", cx.Name, err)
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	cfg := s.cfgSnapshot()
	enabled := tagger.EnabledTaggersForGallery(cfg, cx.Name)
	if len(enabled) == 0 {
		return nil
	}
	ctx, err := s.startScheduledPhase(models.JobTypeAutotag, "autotag", cx.Name)
	if err != nil {
		return err
	}
	baseline := readVmRSS()
	skipped, err := tagger.RunWithTaggers(ctx, cx.DB, cfg, ids, enabled, s.jobs, cfg.Tagger.ExecutionProvider, cx.MangaCacheDir())
	err = s.completeAutotagRun(cx, ctx, "["+cx.Name+"] ", "",
		"scheduled "+cx.Name, len(ids), skipped, baseline, err)
	if err != nil {
		logx.Warnf("scheduler autotag %q: %v", cx.Name, err)
	}
	return err
}

func (s *Server) recordScheduleRun(started time.Time, dur time.Duration, info string) {
	s.sched.recordRun(started, dur, info)
	// Persisted: the catch-up after a restart needs to know whether last
	// night ran.
	s.saveScheduledRun(started)
}

type ScheduleStatus struct {
	LastRun     time.Time
	LastDur     time.Duration
	LastInfo    string
	NextRun     time.Time
	NextRunNote string
	LookupInfo  []string
}

func (s *Server) scheduleStatus() ScheduleStatus {
	st := s.sched.lastRunStatus()
	// This process has not run a pass yet; the stored run is the last one.
	if st.LastRun.IsZero() {
		if last := s.lastScheduledRun(); !last.IsZero() {
			st.LastRun = last.Local()
		}
	}
	if next, ok := s.nextScheduledFire(time.Now()); ok {
		st.NextRun = next
		return st
	}
	sched, galleries := s.scheduleView()
	switch mode := sched.EffectiveMode(); {
	case mode == config.ScheduleOff:
		st.NextRunNote = "No next run scheduled (set to never)."
	case mode == config.ScheduleOnStart:
		st.NextRunNote = "No next run scheduled (it runs at startup only)."
	case !schedHasAnyEnabled(sched):
		st.NextRunNote = "No next run scheduled (every action is off)."
	case !schedHasWork(sched, galleries):
		st.NextRunNote = "No next run scheduled (no gallery is ticked for the actions that are on)."
	default:
		st.NextRunNote = "No next run scheduled (the time of day is unreadable)."
	}
	return st
}
