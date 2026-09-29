package lookup

import "time"

// FlagColumn is per backend: the PTR is free and the boorus spend a quota,
// so an image can be worth one and not the other.
func FlagColumn(backend string) string {
	if backend == BackendPTR {
		return "scheduled_lookup_ptr"
	}
	return "scheduled_lookup"
}

// CandidateClause expects images aliased as i. Only a source with a URL
// disqualifies: a PTR hit writes a url-less one that must not end the
// booru search. No index knows an archive's own hash.
func CandidateClause(backend string) string {
	return `i.is_missing = 0
	  AND i.` + FlagColumn(backend) + ` = 1
	  AND i.file_type <> 'cbz'
	  AND NOT EXISTS (SELECT 1 FROM image_sources s WHERE s.image_id = i.id AND s.url <> '')`
}

// DueClause expects images aliased as i. A PTR retry needs both its delay
// and an index move: the cursor alone re-pulls every miss most days, the
// delay alone re-asks an unchanged index.
func DueClause(backend string, now time.Time) (string, []any) {
	blocked := `l.queued_at IS NOT NULL OR l.next_due_at IS NULL OR l.next_due_at > ?`
	args := []any{backend, stamp(now)}
	if backend == BackendPTR {
		// A cold cursor drops the index gate rather than block every row:
		// over-counting is recoverable, never looking again is not.
		if cursor := PTRCursor.Load(); cursor > 0 {
			blocked += ` OR l.ptr_cursor >= ?`
			args = append(args, int64(cursor))
		}
	}
	return `NOT EXISTS (SELECT 1 FROM image_lookups l
	          WHERE l.image_id = i.id AND l.backend = ? AND (` + blocked + `))`, args
}
