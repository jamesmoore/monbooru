package jobs

import (
	"context"
	"fmt"
)

// Chunked checks ctx only between chunks, and a cancel is not an error.
func Chunked(ctx context.Context, mgr *Manager, ids []int64, chunkSize int, noun string,
	op func(chunk []int64) error,
) (processed int, cancelled bool, err error) {
	total := len(ids)
	if mgr != nil {
		mgr.Update(0, total, fmt.Sprintf("%s…", noun))
	}
	for start := 0; start < total; start += chunkSize {
		if ctx.Err() != nil {
			return processed, true, nil
		}
		end := min(start+chunkSize, total)
		chunk := ids[start:end]
		if err := op(chunk); err != nil {
			return processed, false, err
		}
		processed = end
		if mgr != nil {
			mgr.Update(processed, total, fmt.Sprintf("%s…", noun))
		}
	}
	return processed, false, nil
}
