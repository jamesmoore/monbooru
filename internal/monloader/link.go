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
	l := &Link{cfg: cfg, jobs: jm, Fetches: NewFetchLog()}
	l.client = &Client{Base: l.Base, Token: func() string { return l.cfg().Token }}
	return l
}

// A paused link answers "", so every outbound call fails before any I/O.
func (l *Link) Base() string {
	c := l.cfg()
	if c.Paused {
		return ""
	}
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
	if !l.cfg().Paired {
		return false
	}
	ml := l.Cache.Seed()
	return ml.PTR && ml.Contrib
}

// A syncing index still answers diffs, marked provisional.
func (l *Link) ContribReadOpen() bool {
	if !l.cfg().Paired {
		return false
	}
	ml := l.Cache.Seed()
	return ml.PTR || ml.PTRSyncing
}

func (l *Link) ContribHint() (bool, string) {
	ml := l.Cache.Seed()
	switch {
	case ml.Contrib:
		return true, ""
	case ml.PTRSyncing:
		return false, "the Public Tag Repository is still syncing"
	case ml.ContribBanned:
		return false, "the contribution account is banned"
	default:
		return false, "a contribution account in monloader is required to contribute"
	}
}

// The authed queue read is there to surface a revoked token.
func (l *Link) Probe(ctx context.Context) Status {
	base := strings.TrimRight(l.Base(), "/")
	if base == "" {
		return Status{}
	}
	version, up := peer.Health(ctx, base)
	if !up {
		return Status{Conn: peer.ConnDown}
	}
	st := Status{Conn: peer.ConnOK, Version: version}
	tok := l.cfg().Token
	if tok != "" {
		if qresp, qerr := l.client.Do(ctx, http.MethodGet, "/api/v1/queue?limit=1", nil); qerr == nil {
			defer func() { _ = qresp.Body.Close() }()
			if qresp.StatusCode == http.StatusUnauthorized || qresp.StatusCode == http.StatusForbidden {
				return Status{Conn: peer.ConnRejected, Version: version}
			}
		}
		if presp, perr := l.client.Do(ctx, http.MethodGet, "/api/v1/ptr/status", nil); perr == nil {
			var p struct {
				Enabled  bool `json:"enabled"`
				Progress struct {
					UpdateIndex uint64 `json:"update_index"`
				} `json:"progress"`
				State     string `json:"state"`
				Queryable bool   `json:"queryable"`
				Contrib   *struct {
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
			// One that predates queryable answers only while ready.
			st.PTR = on && (p.Queryable || p.State == "ready")
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
func (l *Link) Refresh(ctx context.Context) (status, version string) {
	if st, ok := l.Cache.Fresh(statusTTL); ok {
		return st.Conn, st.Version
	}
	st := l.Probe(ctx)
	l.Cache.Store(st)
	return st.Conn, st.Version
}
