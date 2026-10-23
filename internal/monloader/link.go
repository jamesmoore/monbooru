package monloader

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// LinkConfig is what the link reads of the live config, on every call.
type LinkConfig struct {
	APIURL  string
	Token   string
	PeerURL string
	Paused  bool
	Paired  bool
}

type Link struct {
	cfg     func() LinkConfig
	client  *Client
	jobs    *jobs.Manager
	Cache   StatusCache
	Fetches *FetchLog
}

func NewLink(cfg func() LinkConfig, jm *jobs.Manager) *Link {
	l := &Link{cfg: cfg, jobs: jm, Fetches: newFetchLog()}
	l.client = &Client{Base: l.Base, Token: func() string { return l.cfg().Token }}
	return l
}

// A paused link answers "", so every outbound call fails before any I/O.
func (l *Link) Base() string {
	if l.cfg().Paused {
		return ""
	}
	return l.addr()
}

func (l *Link) addr() string {
	c := l.cfg()
	if u := strings.TrimSpace(c.APIURL); u != "" {
		return u
	}
	if c.Paired {
		return c.PeerURL
	}
	return ""
}

func (l *Link) Paused() bool { return l.cfg().Paused }

func (l *Link) Status() Status { return l.Cache.Seed() }

// A cold cache ("") counts as up so a fresh boot does not blank the
// buttons before the first probe.
func (l *Link) Usable() bool {
	conn := l.Cache.Seed().Conn
	c := l.cfg()
	if c.Paused {
		return false
	}
	return c.Paired && conn != peer.ConnDown && conn != peer.ConnRejected
}

func (l *Link) PTRReady() bool { return l.Cache.Seed().PTR }

func (l *Link) ContribOpen() bool {
	ml := l.Cache.Seed()
	return l.cfg().Paired && ml.PTR && ml.Contrib
}

// A syncing index still answers diffs, marked provisional.
func (l *Link) ContribReadOpen() bool {
	ml := l.Cache.Seed()
	return l.cfg().Paired && (ml.PTR || ml.PTRSyncing)
}

func (l *Link) ContribHint() (bool, string) {
	ml := l.Cache.Seed()
	switch {
	case ml.Contrib:
		return true, ""
	case ml.PTRSyncing:
		return false, contribRefusals[contract.CodePTRSyncing]
	case ml.ContribBanned:
		return false, contribRefusals[contract.CodePTRBanned]
	default:
		return false, contribRefusals[contract.CodePTRAccountRequired]
	}
}

var contribRefusals = map[string]string{
	contract.CodePTRSyncing:         "the Public Tag Repository is still syncing",
	contract.CodePTRBanned:          "the contribution account is banned",
	contract.CodePTRAccountRequired: "a contribution account in monloader is required to contribute",
}

// The authed queue read is there to surface a revoked token.
func (l *Link) Probe(ctx context.Context) Status {
	base := strings.TrimRight(l.Base(), "/")
	if base == "" {
		return Status{}
	}
	h, up := peer.Health(ctx, base)
	if !up {
		return Status{Conn: peer.ConnDown}
	}
	st := Status{Conn: peer.ConnOK, Version: h.Version, APIMismatch: h.APIMismatch(contract.AppMonbooru, peer.APIVersion, peer.MinPeerAPI)}
	tok := l.cfg().Token
	if tok != "" {
		if qresp, qerr := l.client.Do(ctx, http.MethodGet, "/api/v1/queue?limit=1", nil); qerr == nil {
			defer func() { _ = qresp.Body.Close() }()
			if c := qresp.StatusCode; c == http.StatusUnauthorized || c == http.StatusForbidden || c == http.StatusServiceUnavailable || setupRequired(qresp) {
				st.Conn = peer.ConnRejected
				return st
			}
		}
		if presp, perr := l.client.Do(ctx, http.MethodGet, "/api/v1/ptr/status", nil); perr == nil {
			var p ptrStatus
			_ = json.NewDecoder(presp.Body).Decode(&p)
			_ = presp.Body.Close()
			on := presp.StatusCode == http.StatusOK && p.Enabled
			// The lookup:due filter reads the cursor to skip images whose
			// PTR miss the index has not moved past.
			if p.Progress.UpdateIndex > 0 {
				lookup.PTRCursor.Store(p.Progress.UpdateIndex)
			}
			// monloader refuses every PTR read until its index is caught up.
			// One that predates queryable answers only while ready.
			st.PTR = on && (p.Queryable || p.State == contract.PTRReady)
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

// Under the light's 15s poll cadence, so a page left open still refreshes
// on schedule.
const statusTTL = 10 * time.Second

// The probe runs unlocked so a slow monloader never serializes page renders.
func (l *Link) Refresh(ctx context.Context) Status {
	if st, ok := l.Cache.Fresh(statusTTL); ok {
		return st
	}
	st := l.Probe(ctx)
	if st.APIMismatch != "" && st.APIMismatch != l.Cache.Seed().APIMismatch {
		logx.Warnf("monloader: %s", st.APIMismatch)
	}
	l.Cache.Store(st)
	return st
}
