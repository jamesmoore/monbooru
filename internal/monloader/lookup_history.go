package monloader

import (
	"context"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/lookup"
)

// Comfortably past a real backlog, so a busy queue is never mistaken for
// a lost callback.
const lookupGrace = 6 * time.Hour

// Only logs a history failure: the job is already queued on monloader.
func (l *Link) EnqueueLookup(ctx context.Context, cx *library.Gallery, imageID int64, backend, md5, sha256 string, background, budgeted bool) error {
	jobID, err := l.EnqueueHashLookup(ctx, imageID, cx.Name, backend, md5, sha256, background, budgeted)
	if err != nil {
		return err
	}
	if err := cx.Edit(func(d *db.DB) error {
		return lookup.Enqueued(d, imageID, lookup.Backends(backend), jobID, time.Now())
	}); err != nil {
		logx.Warnf("lookup history for image %d: %v", imageID, err)
	}
	return nil
}

// Never from a render path: a monloader round trip there would spend the
// page's latency budget on a link that may be down.
func (l *Link) reconcileLookups(ctx context.Context, cx *library.Gallery) (resolved, inconclusive int) {
	waiting, err := lookup.Waiting(cx.DB, time.Now().Add(-lookupGrace))
	if err != nil {
		logx.Warnf("lookup reconcile %q: %v", cx.Name, err)
		return 0, 0
	}
	for _, f := range waiting {
		if ctx.Err() != nil {
			return resolved, inconclusive
		}
		result, ok := l.JobOutcome(ctx, f.JobID)
		if !ok {
			continue
		}
		if result == "" {
			// Evidence about the plumbing, not the image: the ladder
			// stays and the row is due now.
			inconclusive++
			result = lookup.ResultError
		}
		if err := cx.Edit(func(d *db.DB) error { return lookup.Record(d, f.ImageID, f.Backend, result, 0, time.Now()) }); err != nil {
			logx.Warnf("lookup reconcile %q image %d: %v", cx.Name, f.ImageID, err)
			continue
		}
		resolved++
	}
	return resolved, inconclusive
}

func (l *Link) ReconcileAll(galleries []*library.Gallery) {
	if !l.Usable() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, cx := range galleries {
		l.reconcileLookups(ctx, cx)
	}
}
