package monloader

import "github.com/monbooru/monbooru/internal/lookup"

// Fetch states a detail page's fetch status passes through: pending and
// ok are monbooru's own, the rest arrive from monloader's fetch-status
// report, a failure as one of its queue error codes.
const (
	StatePending       = "pending"
	StateOK            = "ok"
	StateHashNotFound  = "hash_not_found"
	StateCanceled      = "canceled"
	StateAlreadyExists = "already_exists"
)

const (
	CodeUnsupportedURL      = "unsupported_url"
	CodeNetworkUnreachable  = "network_unreachable"
	CodeAuthRequired        = "auth_required"
	CodeBlocked             = "blocked"
	CodeRateLimited         = "rate_limited"
	CodeDownloadFailed      = "download_failed"
	CodeMonbooruUnreachable = "monbooru_unreachable"
	CodeMonbooruRejected    = "monbooru_rejected"
	CodeMappingFailed       = "mapping_failed"
)

// Job statuses and the item outcome of GET /api/v1/queue/{id}, and the
// job kind a lookup's fetch-status report names.
const (
	JobQueued      = "queued"
	JobRunning     = "running"
	JobFailed      = "failed"
	JobCanceled    = "canceled"
	JobInterrupted = "interrupted"

	ItemEnriched = "enriched"

	KindLookup = "lookup"
)

// Names a lookup trail gives its backends. PTRTrailName must match
// monloader's ptrTrailName.
const (
	PTRTrailName  = "Public Tag Repository"
	TrailIQDB     = "iqdb"
	TrailSauceNAO = "saucenao"
)

// Outcome reads a fetch state as a lookup result. Only hash_not_found says
// anything about the image; a dropped job or a failure code is about the
// plumbing and leaves the ladder where it was.
func Outcome(state string) (result string, concluded bool) {
	switch state {
	case StatePending, StateOK:
		return "", false
	case StateHashNotFound:
		return lookup.ResultMiss, true
	}
	return lookup.ResultError, true
}
