package monloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/peer"
)

// A 409 on a PTR call: the cached capability was stale; the link itself
// is fine.
var ErrPTRUnavailable = errors.New("the PTR lookup is unavailable on monloader")

// It ends the phase: every later row would get the same answer.
var ErrBudgetSpent = errors.New("monloader's daily lookup budget is spent")

// No per-image fallback: one queued job per image is what the batch
// endpoint exists to stop.
var ErrPTRBatchUnsupported = errors.New("monloader is too old for batch PTR lookup")

// A 404 from a monloader too old to search; the dialog falls back to its
// plain input.
var ErrNoSearch = errors.New("monloader does not serve the spelling search")

var ErrSearchUnbounded = errors.New("a substring search needs a category")

// monbooru only enqueues, keeping its single-egress model: every fetch
// runs on monloader.
func (l *Link) enqueue(ctx context.Context, path string, payload map[string]any, onConflict error) (int64, error) {
	resp, err := l.client.Post(ctx, path, payload)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if onConflict != nil && resp.StatusCode == http.StatusConflict {
		return 0, onConflict
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, ErrBudgetSpent
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return 0, peer.StatusError{Peer: App, Status: resp.Status}
	}
	var out struct {
		JobID int64 `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.JobID, nil
}

func (l *Link) EnqueueMetadataFetch(ctx context.Context, imageID int64, gallery, url string) error {
	_, err := l.enqueue(ctx, "/api/v1/metadata",
		map[string]any{"image_id": imageID, "gallery": gallery, "url": url}, nil)
	return err
}

func (l *Link) EnqueueReplace(ctx context.Context, imageID int64, gallery, url string) error {
	_, err := l.enqueue(ctx, "/api/v1/replace",
		map[string]any{"image_id": imageID, "gallery": gallery, "url": url}, nil)
	return err
}

func (l *Link) EnqueueHashLookup(ctx context.Context, imageID int64, gallery, backend, md5, sha256 string, background, budgeted bool) (int64, error) {
	return l.enqueue(ctx, "/api/v1/lookup", map[string]any{
		"image_id": imageID, "gallery": gallery, "backend": backend, "md5": md5, "sha256": sha256,
		"background": background, "budgeted": budgeted,
	}, ErrPTRUnavailable)
}

func (l *Link) GalleryRenamed(ctx context.Context, oldName, newName string) error {
	if !l.cfg().Paired {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := l.client.Post(ctx, "/api/v1/galleries/rename", map[string]any{"from": oldName, "to": newName})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return peer.StatusError{Peer: App, Status: resp.Status}
	}
	return nil
}

// Teardown runs remove, which drops the pairing and its token, then tells
// monloader with the address and token read before it.
func (l *Link) Teardown(remove func()) error {
	base, token := l.Base(), l.cfg().Token
	remove()
	err := peer.NotifyTeardown(App, base, token)
	if err != nil {
		logx.Errorf("pairing: could not notify monloader of teardown: %v", err)
	}
	return err
}

type PTRLookupImage struct {
	ImageID int64  `json:"image_id"`
	SHA256  string `json:"sha256"`
}

func (l *Link) PTRBatchLookup(ctx context.Context, gallery string, scheduled bool, images []PTRLookupImage) (map[string][]string, uint64, error) {
	resp, err := l.client.Post(ctx, "/api/v1/ptr/lookup", map[string]any{
		"images": images, "gallery": gallery, "scheduled": scheduled,
	})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return nil, 0, ErrPTRUnavailable
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, 0, ErrPTRBatchUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, peer.StatusError{Peer: App, Status: resp.Status}
	}
	var out struct {
		Index   uint64              `json:"index"`
		Results map[string][]string `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, 0, err
	}
	return out.Results, out.Index, nil
}

func (l *Link) PTRIndexCursor(ctx context.Context) (uint64, error) {
	resp, err := l.client.Do(ctx, http.MethodGet, "/api/v1/ptr/status", nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, peer.StatusError{Peer: App, Status: resp.Status}
	}
	var out struct {
		Progress struct {
			UpdateIndex uint64 `json:"update_index"`
		} `json:"progress"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Progress.UpdateIndex, nil
}

// false leaves the row in flight. An empty result is inconclusive: moving
// the ladder on it would walk an image to "nothing found" while monloader
// is down.
func (l *Link) JobOutcome(ctx context.Context, jobID int64) (string, bool) {
	if jobID == 0 {
		return "", true // a monloader that reports no job id
	}
	resp, err := l.client.Do(ctx, http.MethodGet, "/api/v1/queue/"+strconv.FormatInt(jobID, 10), nil)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		// Aged out of the finished ring, or never existed.
		return "", true
	}
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var job struct {
		Status  string `json:"status"`
		Backend string `json:"backend"`
		Items   []struct {
			Outcome   string `json:"outcome"`
			ErrorCode string `json:"error_code"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return "", false
	}
	switch job.Status {
	case JobQueued, JobRunning:
		return "", false
	case JobCanceled, JobInterrupted:
		return "", true
	case JobFailed:
		return lookup.ResultError, true
	}
	for _, it := range job.Items {
		if it.Outcome == ItemEnriched {
			// A job over both backends cannot say which one matched; that
			// one's enrich already concluded its own row.
			if len(lookup.Backends(job.Backend)) > 1 {
				return "", true
			}
			return lookup.ResultHit, true
		}
		if result, _ := Outcome(it.ErrorCode); result == lookup.ResultMiss {
			return result, true
		}
	}
	return lookup.ResultError, true
}

func postJSON[T any](l *Link, ctx context.Context, path string, payload map[string]any) (*T, error) {
	body, _ := json.Marshal(payload)
	resp, err := l.client.Do(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return nil, ErrPTRUnavailable
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("monloader returned %s", resp.Status)
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Names are in monbooru form (bare or category:name).
type PTRTagInfo struct {
	Known        bool     `json:"known"`
	Ideal        string   `json:"ideal"`
	Aliases      []string `json:"aliases"`
	Implications []string `json:"implications"`
	ImpliedBy    []string `json:"implied_by"`
}

// The caller keeps a call's names under monloader's request limit.
func (l *Link) PTRTagLookup(ctx context.Context, names []string) (map[string]PTRTagInfo, error) {
	out, err := postJSON[struct {
		Results map[string]PTRTagInfo `json:"results"`
	}](l, ctx, "/api/v1/ptr/tags", map[string]any{"tags": names})
	if err != nil {
		return nil, err
	}
	return out.Results, nil
}

type PTRCluster struct {
	Ideal        string   `json:"ideal"`
	Matched      []string `json:"matched"`
	Aliases      int      `json:"aliases"`
	Implications int      `json:"implications"`
	ImpliedBy    int      `json:"implied_by"`
}

func (l *Link) PTRSpellingSearch(ctx context.Context, q, mode string, limit int) (clusters []PTRCluster, truncated bool, err error) {
	v := url.Values{"q": {q}, "limit": {strconv.Itoa(limit)}}
	if mode != "" {
		v.Set("mode", mode)
	}
	resp, err := l.client.Do(ctx, http.MethodGet, "/api/v1/ptr/tags/search?"+v.Encode(), nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, false, ErrNoSearch
	case http.StatusBadRequest:
		return nil, false, ErrSearchUnbounded
	case http.StatusConflict:
		return nil, false, ErrPTRUnavailable
	default:
		return nil, false, fmt.Errorf("monloader returned %s", resp.Status)
	}
	var out struct {
		Clusters  []PTRCluster `json:"clusters"`
		Truncated bool         `json:"truncated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false, err
	}
	return out.Clusters, out.Truncated, nil
}

type ContribPreviewToAdd struct {
	Tag        string `json:"tag"`
	PTR        string `json:"ptr"`
	Status     string `json:"status"`
	Note       string `json:"note"`
	UnknownTag bool   `json:"unknown_tag"`
	Color      string `json:"-"`
	// Sent is the spelling the row goes up under; Tag is the one the
	// operator sees.
	Sent string `json:"-"`
}

func (t ContribPreviewToAdd) PTRDiffers() bool { return strings.ReplaceAll(t.Tag, "_", " ") != t.PTR }

type ContribPreviewPTROnly struct {
	Tag          string `json:"tag"`
	PTR          string `json:"ptr"`
	Petitionable bool   `json:"petitionable"`
	Color        string `json:"-"`
}

type ContribPreview struct {
	Provisional bool                    `json:"provisional"`
	ToAdd       []ContribPreviewToAdd   `json:"to_add"`
	PTROnly     []ContribPreviewPTROnly `json:"ptr_only"`
}

func (l *Link) PreviewContrib(ctx context.Context, sha256 string, tags, implied []string) (*ContribPreview, error) {
	return postJSON[ContribPreview](l, ctx, "/api/v1/ptr/contrib/preview",
		map[string]any{"sha256": sha256, "tags": tags, "implied": implied})
}

type ContribSendResult struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
	Note   string `json:"note"`
}

type ContribSendResponse struct {
	Results []ContribSendResult `json:"results"`
	JobID   int64               `json:"job_id"`
}

func (l *Link) SendContrib(ctx context.Context, origin string, items []map[string]any) (*ContribSendResponse, error) {
	return postJSON[ContribSendResponse](l, ctx, "/api/v1/ptr/contrib",
		map[string]any{"commit": true, "origin": origin, "items": items})
}

type PairPreview struct {
	APtr        string `json:"a_ptr"`
	BPtr        string `json:"b_ptr"`
	Direction   string `json:"direction"`
	Note        string `json:"note"`
	Provisional bool   `json:"provisional"`
}

func (l *Link) PreviewPair(ctx context.Context, kind, a, b string) (*PairPreview, error) {
	return postJSON[PairPreview](l, ctx, "/api/v1/ptr/contrib/pair-preview",
		map[string]any{"kind": kind, "a": a, "b": b})
}
