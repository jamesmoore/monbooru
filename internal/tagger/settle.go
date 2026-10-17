package tagger

import (
	"context"
	"fmt"

	"github.com/monbooru/monbooru/internal/jobs"
)

// Settle ends an autotag job; finished reports a run that went to the end,
// whose peak memory the caller may log.
func Settle(ctx context.Context, mgr *jobs.Manager, prefix, itemNoun string, total, skipped int, err error) (finished bool, _ error) {
	done := fmt.Sprintf("%sauto-tagged %d %simage(s)", prefix, total, itemNoun)
	if skipped > 0 {
		done = fmt.Sprintf("%sauto-tagged %d of %d %simage(s), %d skipped", prefix, total-skipped, total, itemNoun, skipped)
	}
	finished = ctx.Err() == nil && err == nil
	return finished, mgr.Settle(ctx, err, fmt.Sprintf("%sauto-tagging cancelled (%d image(s) queued)", prefix, total), done)
}

func SettleImage(ctx context.Context, mgr *jobs.Manager, id int64, skipped int, err error) (finished bool, _ error) {
	done := fmt.Sprintf("auto-tagged image #%d", id)
	if skipped > 0 {
		done = fmt.Sprintf("auto-tagger skipped image #%d", id)
	}
	finished = ctx.Err() == nil && err == nil
	return finished, mgr.Settle(ctx, err, "auto-tagging cancelled", done)
}
