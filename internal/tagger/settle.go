package tagger

import (
	"context"
	"fmt"

	"github.com/monbooru/monbooru/internal/jobs"
)

// Settle ends an autotag job; finished reports a run that went to the end,
// whose peak memory the caller may log.
func Settle(ctx context.Context, mgr *jobs.Manager, prefix, itemNoun string, total, skipped int, err error) (finished bool, _ error) {
	if ctx.Err() != nil {
		mgr.Complete(fmt.Sprintf("%sauto-tagging cancelled (%d image(s) queued)", prefix, total))
		return false, nil
	}
	if err != nil {
		mgr.Fail(err.Error())
		return false, err
	}
	if skipped > 0 {
		mgr.Complete(fmt.Sprintf("%sauto-tagged %d of %d %simage(s), %d skipped", prefix, total-skipped, total, itemNoun, skipped))
		return true, nil
	}
	mgr.Complete(fmt.Sprintf("%sauto-tagged %d %simage(s)", prefix, total, itemNoun))
	return true, nil
}

func SettleImage(ctx context.Context, mgr *jobs.Manager, id int64, skipped int, err error) (finished bool, _ error) {
	if ctx.Err() != nil {
		mgr.Complete("auto-tagging cancelled")
		return false, nil
	}
	if err != nil {
		mgr.Fail(err.Error())
		return false, err
	}
	if skipped > 0 {
		mgr.Complete(fmt.Sprintf("auto-tagger skipped image #%d", id))
		return true, nil
	}
	mgr.Complete(fmt.Sprintf("auto-tagged image #%d", id))
	return true, nil
}
