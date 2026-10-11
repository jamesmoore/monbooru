// Package peer is what monloader and the plugins share: the pending pairing
// requests, the grant and revoke written to the config, the address rules,
// the /health probe and the teardown notice.
package peer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/config"
)

// Companion is the companion's name in pairing records; its credential and
// pause live in [monloader], every other peer's in a [[plugin]] block.
const Companion = "monloader"

const (
	pendingTTL = 5 * time.Minute
	maxPending = 16
)

type State string

const (
	Pending  State = "pending"
	Approved State = "approved"
	Denied   State = "denied"
)

// Nothing is issued until the claim, so an approval the peer never
// collects mints no token.
type Req struct {
	ID        string
	App       string
	URL       string
	Source    string
	Scopes    []string
	PeerToken string
	Version   string
	Buttons   []config.PluginButton
	State     State
	Claimed   bool
	CreatedAt time.Time
	Repair    bool
}

// An open button's page runs on monbooru's origin with the operator's
// session, so without write it grants more than the scopes say.
func (r Req) OpenOutranksScopes() bool {
	return !slices.Contains(r.Scopes, config.ScopeWrite) &&
		slices.ContainsFunc(r.Buttons, func(b config.PluginButton) bool { return b.Mode == config.ModeOpen })
}

type Store struct {
	mu sync.Mutex
	m  map[string]*Req
}

func NewStore() *Store { return &Store{m: map[string]*Req{}} }

func pairID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (ps *Store) sweepLocked() {
	cutoff := time.Now().Add(-pendingTTL)
	maps.DeleteFunc(ps.m, func(_ string, r *Req) bool {
		return r.CreatedAt.Before(cutoff)
	})
}

// A second request from the same app replaces its pending one: the
// endpoint is unauthenticated, and one noisy peer would otherwise fill
// the cap for the TTL.
func (ps *Store) Create(req Req) (string, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.sweepLocked()
	maps.DeleteFunc(ps.m, func(_ string, r *Req) bool {
		return r.State == Pending && r.App == req.App
	})
	pending := 0
	for _, r := range ps.m {
		if r.State == Pending {
			pending++
		}
	}
	if pending >= maxPending {
		return "", false
	}
	req.ID, req.State, req.CreatedAt = pairID(), Pending, time.Now()
	ps.m[req.ID] = &req
	return req.ID, true
}

// A plugin stopped by a removal may offer to pair on its way out; a card
// for it is noise.
func (ps *Store) DropPending(app string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	maps.DeleteFunc(ps.m, func(_ string, r *Req) bool {
		return r.State == Pending && r.App == app
	})
}

func (ps *Store) ListPending() []Req {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.sweepLocked()
	var out []Req
	for _, r := range ps.m {
		if r.State == Pending {
			out = append(out, *r)
		}
	}
	return out
}

func (ps *Store) Get(id string) (Req, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if r, ok := ps.m[id]; ok {
		return *r, true
	}
	return Req{}, false
}

func (ps *Store) SetState(id string, st State) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r, ok := ps.m[id]
	if !ok || r.State != Pending {
		return false
	}
	r.State = st
	return true
}

func (ps *Store) Claim(id string) (Req, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r, ok := ps.m[id]
	if !ok || r.State != Approved || r.Claimed {
		return Req{}, false
	}
	r.Claimed = true
	return *r, true
}

func (ps *Store) Unclaim(id string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if r, ok := ps.m[id]; ok {
		r.Claimed = false
	}
}

func (ps *Store) Remove(id string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.m, id)
}

func ValidateOffer(app, version string, buttons []config.PluginButton) error {
	if err := config.ValidatePluginName(app); err != nil {
		return err
	}
	if utf8.RuneCountInString(version) > config.MaxPluginVersion {
		return fmt.Errorf("version must be at most %d characters", config.MaxPluginVersion)
	}
	return config.ValidatePluginButtons(buttons)
}

// A peer advertises its own base_url, whose host (usually localhost) means
// nothing from here; the request's source is where it can be reached.
func CallbackURL(advertised, source string) string {
	source = strings.TrimSpace(source)
	u, err := url.Parse(strings.TrimSpace(advertised))
	if err != nil || u.Host == "" || source == "" {
		return advertised
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(source, port)
	} else {
		u.Host = source
	}
	return u.String()
}

func Repoint(c *config.Config, app, url string) {
	if t := c.FindPairedToken(app); t != nil {
		t.PeerURL = url
	}
}

// The request stores the grant, so the approval card shows what the
// token will carry.
func GrantedScopes(requested []string) []string {
	if scopes := config.FilterScopes(requested); len(scopes) > 0 {
		return scopes
	}
	return []string{config.ScopeRead, config.ScopeWrite}
}

// Grant stores tok, minted for req, and the peer's own credential.
func Grant(c *config.Config, req Req, tok config.Token) {
	// A re-pair replaces the credentials: the copy that held the old
	// token is gone.
	c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t config.Token) bool { return t.Paired == req.App })
	c.Auth.Tokens = append(c.Auth.Tokens, tok)
	if req.App == Companion {
		if c.EnvPinned("monloader.api_token") == "" {
			c.Monloader.APIToken = req.PeerToken
		}
		return
	}
	p := c.FindPlugin(req.App)
	if p == nil {
		c.Plugins = append(c.Plugins, config.PluginConfig{Name: req.App})
		p = &c.Plugins[len(c.Plugins)-1]
	}
	p.Version, p.PeerToken, p.Buttons, p.Paused = req.Version, req.PeerToken, req.Buttons, false
}

func SetPaused(c *config.Config, app string, paused bool) {
	if app == Companion {
		c.Monloader.Paused = paused
		return
	}
	if p := c.FindPlugin(app); p != nil {
		p.Paused = paused
	}
}

// Keeps the configured api_url so an operator's URL survives an unpair
// and re-pair.
func Revoke(c *config.Config, app string) {
	c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t config.Token) bool { return t.Paired == app })
	if app == Companion {
		if c.EnvPinned("monloader.api_token") == "" {
			c.Monloader.APIToken = ""
		}
		return
	}
	if p := c.FindPlugin(app); p != nil {
		p.PeerToken, p.Version, p.Buttons = "", "", nil
	}
	// A block with the operator's own lines stays, since config.Save
	// re-encodes the whole file from the struct. Enabled counts: dropping
	// it would leave a running plugin whose row offers to enable it.
	c.Plugins = slices.DeleteFunc(c.Plugins, func(p config.PluginConfig) bool {
		return p.Name == app && p.APIURL == "" && !p.Enabled
	})
}

func OverrideURL(c *config.Config, app string) string {
	if app == Companion {
		return strings.TrimSpace(c.Monloader.APIURL)
	}
	if p := c.FindPlugin(app); p != nil {
		return strings.TrimSpace(p.APIURL)
	}
	return ""
}
