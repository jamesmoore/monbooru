package web

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

	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/monloader"
)

// Only ever issues the unauthed /health probe, hence its own short timeout.
var peerHTTPClient = &http.Client{Timeout: 5 * time.Second}

// monbooru only enqueues, keeping its single-egress model: every fetch
// runs on monloader.
func (s *Server) enqueueMonloader(ctx context.Context, path string, payload map[string]any, onConflict error) (int64, error) {
	resp, err := s.monloader().Post(ctx, path, payload)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if onConflict != nil && resp.StatusCode == http.StatusConflict {
		return 0, onConflict
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, errLookupBudgetSpent
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return 0, peerStatusError{monloaderApp, resp.Status}
	}
	var out struct {
		JobID int64 `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.JobID, nil
}

func (s *Server) enqueueMetadataFetch(ctx context.Context, imageID int64, gallery, url string) error {
	_, err := s.enqueueMonloader(ctx, "/api/v1/metadata",
		map[string]any{"image_id": imageID, "gallery": gallery, "url": url}, nil)
	return err
}

func (s *Server) enqueueReplace(ctx context.Context, imageID int64, gallery, url string) error {
	_, err := s.enqueueMonloader(ctx, "/api/v1/replace",
		map[string]any{"image_id": imageID, "gallery": gallery, "url": url}, nil)
	return err
}

// A 409 on a PTR call: the cached capability was stale; the link itself
// is fine.
var errPTRUnavailable = errors.New("the PTR lookup is unavailable on monloader")

// It ends the phase: every later row would get the same answer.
var errLookupBudgetSpent = errors.New("monloader's daily lookup budget is spent")

// No per-image fallback: one queued job per image is what the batch
// endpoint exists to stop.
var errPTRBatchUnsupported = errors.New("monloader is too old for batch PTR lookup")

// A refused request, not a sign the peer is down, so a batch skips the
// row. It carries the peer's name: a plugin's refusal must not read as
// monloader's.
type peerStatusError struct{ peer, status string }

func (e peerStatusError) Error() string { return e.peer + " returned " + e.status }

func isPeerStatusErr(err error) bool {
	var se peerStatusError
	return errors.As(err, &se)
}

func (s *Server) enqueueHashLookup(ctx context.Context, imageID int64, gallery, backend, md5, sha256 string, background, budgeted bool) (int64, error) {
	return s.enqueueMonloader(ctx, "/api/v1/lookup", map[string]any{
		"image_id": imageID, "gallery": gallery, "backend": backend, "md5": md5, "sha256": sha256,
		"background": background, "budgeted": budgeted,
	}, errPTRUnavailable)
}

func (s *Server) monloaderGalleryRenamed(ctx context.Context, oldName, newName string) error {
	if !s.pairedWith(monloaderApp) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := s.monloader().Post(ctx, "/api/v1/galleries/rename", map[string]any{"from": oldName, "to": newName})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return peerStatusError{monloaderApp, resp.Status}
	}
	return nil
}

type ptrLookupImage struct {
	ImageID int64  `json:"image_id"`
	SHA256  string `json:"sha256"`
}

func (s *Server) ptrBatchLookup(ctx context.Context, gallery string, scheduled bool, images []ptrLookupImage) (map[string][]string, uint64, error) {
	resp, err := s.monloader().Post(ctx, "/api/v1/ptr/lookup", map[string]any{
		"images": images, "gallery": gallery, "scheduled": scheduled,
	})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return nil, 0, errPTRUnavailable
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, 0, errPTRBatchUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, peerStatusError{monloaderApp, resp.Status}
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

// Names are in monbooru form (bare or category:name).
type ptrTagInfo struct {
	Known        bool     `json:"known"`
	Ideal        string   `json:"ideal"`
	Aliases      []string `json:"aliases"`
	Implications []string `json:"implications"`
	ImpliedBy    []string `json:"implied_by"`
}

// At most ptrLookupBatch names per call: monloader's request limit.
func (s *Server) ptrTagLookup(ctx context.Context, names []string) (map[string]ptrTagInfo, error) {
	out, err := monloaderPostJSON[struct {
		Results map[string]ptrTagInfo `json:"results"`
	}](s, ctx, "/api/v1/ptr/tags", map[string]any{"tags": names})
	if err != nil {
		return nil, err
	}
	return out.Results, nil
}

type ptrCluster struct {
	Ideal        string   `json:"ideal"`
	Matched      []string `json:"matched"`
	Aliases      int      `json:"aliases"`
	Implications int      `json:"implications"`
	ImpliedBy    int      `json:"implied_by"`
}

// A 404 from a monloader too old to search; the dialog falls back to its
// plain input.
var errPTRNoSearch = errors.New("monloader does not serve the spelling search")

var errPTRSearchUnbounded = errors.New("a substring search needs a category")

func (s *Server) ptrSpellingSearch(ctx context.Context, q, mode string, limit int) (clusters []ptrCluster, truncated bool, err error) {
	v := url.Values{"q": {q}, "limit": {strconv.Itoa(limit)}}
	if mode != "" {
		v.Set("mode", mode)
	}
	resp, err := s.monloader().Do(ctx, http.MethodGet, "/api/v1/ptr/tags/search?"+v.Encode(), nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, false, errPTRNoSearch
	case http.StatusBadRequest:
		return nil, false, errPTRSearchUnbounded
	case http.StatusConflict:
		return nil, false, errPTRUnavailable
	default:
		return nil, false, fmt.Errorf("monloader returned %s", resp.Status)
	}
	var out struct {
		Clusters  []ptrCluster `json:"clusters"`
		Truncated bool         `json:"truncated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false, err
	}
	return out.Clusters, out.Truncated, nil
}

// A paused link answers "", so every outbound call fails before any I/O.
func (s *Server) monloaderAPIBase() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.cfg.Monloader.Paused {
		return ""
	}
	if u := strings.TrimSpace(s.cfg.Monloader.APIURL); u != "" {
		return u
	}
	if t := s.cfg.FindPairedToken(monloaderApp); t != nil {
		return t.PeerURL
	}
	return ""
}

// A cold cache ("") counts as up so a fresh boot does not blank the
// buttons before the first probe.
func (s *Server) monloaderUsable() bool {
	conn := s.mlStatus.Seed().Conn
	if s.monloaderPaused() {
		return false
	}
	return s.pairedWith(monloaderApp) && conn != "down" && conn != "rejected"
}

func (s *Server) monloaderPaused() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Monloader.Paused
}

// The authed queue read is there to surface a revoked token.
func (s *Server) checkMonloader(ctx context.Context) monloader.Status {
	base := strings.TrimRight(s.monloaderAPIBase(), "/")
	if base == "" {
		return monloader.Status{}
	}
	version, up := probePeer(ctx, peerHTTPClient, base)
	if !up {
		return monloader.Status{Conn: "down"}
	}
	st := monloader.Status{Conn: "ok", Version: version}
	s.cfgMu.RLock()
	tok := s.cfg.Monloader.APIToken
	s.cfgMu.RUnlock()
	if tok != "" {
		if qresp, qerr := s.monloader().Do(ctx, http.MethodGet, "/api/v1/queue?limit=1", nil); qerr == nil {
			defer func() { _ = qresp.Body.Close() }()
			if qresp.StatusCode == http.StatusUnauthorized || qresp.StatusCode == http.StatusForbidden {
				return monloader.Status{Conn: "rejected", Version: version}
			}
		}
		if presp, perr := s.monloader().Do(ctx, http.MethodGet, "/api/v1/ptr/status", nil); perr == nil {
			var p struct {
				Enabled  bool `json:"enabled"`
				Progress struct {
					UpdateIndex uint64 `json:"update_index"`
				} `json:"progress"`
				State   string `json:"state"`
				Contrib *struct {
					Account bool `json:"account"`
					Banned  bool `json:"banned"`
					Failed  int  `json:"failed"`
				} `json:"contrib"`
			}
			_ = json.NewDecoder(presp.Body).Decode(&p)
			_ = presp.Body.Close()
			on := presp.StatusCode == http.StatusOK && p.Enabled
			// The lookup:due filter reads the cursor to skip images whose
			// PTR miss the index has not moved past.
			if p.Progress.UpdateIndex > 0 {
				lookup.PTRCursor.Store(p.Progress.UpdateIndex)
			}
			// monloader refuses every PTR read until its index is caught up.
			st.PTR = on && p.State == "ready"
			st.PTRSyncing = on && !st.PTR
			// Gated on a synced index so nothing is contributed against a
			// stale copy.
			st.Contrib = st.PTR && p.Contrib != nil && p.Contrib.Account && !p.Contrib.Banned
			st.ContribBanned = on && p.Contrib != nil && p.Contrib.Banned
			if p.Contrib != nil {
				st.ContribFailed = p.Contrib.Failed
			}
		}
	}
	return st
}

func (s *Server) monloaderReachable(ctx context.Context, base string) bool {
	if strings.TrimRight(base, "/") == "" {
		return false
	}
	_, up := probePeer(ctx, peerHTTPClient, base)
	return up
}

// Under the light's 15s poll cadence, so a page left open still refreshes
// on schedule.
const monloaderStatusTTL = 10 * time.Second

// The probe runs unlocked so a slow monloader never serializes page renders.
func (s *Server) monloaderStatusCached(ctx context.Context) (status, version string) {
	if st, ok := s.mlStatus.Fresh(monloaderStatusTTL); ok {
		return st.Conn, st.Version
	}
	st := s.checkMonloader(ctx)
	s.mlStatus.Store(st)
	return st.Conn, st.Version
}

func (s *Server) monloaderStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !s.pairedWith(monloaderApp) {
		// Stop polling and clear the light once the pairing is gone.
		_, _ = w.Write([]byte(`<span id="monloader-light"></span>`))
		return
	}
	if s.monloaderPaused() {
		s.renderMonloaderLight(w, r, "paused", "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	before := s.mlStatus.Seed()
	status, version := s.monloaderStatusCached(ctx)
	// Panels seeded from stale flags, empty on a fresh session, re-mount
	// on this event.
	after := s.mlStatus.Seed()
	if before.PTR != after.PTR || before.PTRSyncing != after.PTRSyncing ||
		before.Contrib != after.Contrib || before.ContribFailed != after.ContribFailed {
		w.Header().Set("HX-Trigger", "monloader-status-changed")
	}
	s.renderTemplate(w, "partials/monloader_light.html", map[string]any{
		"MonloaderConn":    status,
		"MonloaderVersion": version,
		"MonloaderURL":     s.monloaderWebBase(),
		"CSRFToken":        s.csrfToken(sessionFromContext(r.Context())),
	})
}

func (s *Server) monloader() *monloader.Client {
	return &monloader.Client{
		Base: s.monloaderAPIBase,
		Token: func() string {
			s.cfgMu.RLock()
			defer s.cfgMu.RUnlock()
			return s.cfg.Monloader.APIToken
		},
	}
}
