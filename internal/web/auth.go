package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type contextKey int

const sessionContextKey contextKey = 1

type Session struct {
	ID        string
	ExpiresAt time.Time
}

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
}

func NewSessionStore() *SessionStore { return &SessionStore{sessions: map[string]Session{}} }

func (s *SessionStore) NewSession(lifetimeDays int) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	s.sessions[id] = Session{
		ID:        id,
		ExpiresAt: time.Now().Add(time.Duration(lifetimeDays) * 24 * time.Hour),
	}
	s.mu.Unlock()

	return id, nil
}

func (s *SessionStore) GetSession(id string) (Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()

	if !ok || time.Now().After(sess.ExpiresAt) {
		return Session{}, false
	}
	return sess, true
}

func (s *SessionStore) DeleteSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *SessionStore) Clear() {
	s.mu.Lock()
	s.sessions = map[string]Session{}
	s.mu.Unlock()
}

func (s *SessionStore) ClearExcept(keep string) {
	s.mu.Lock()
	maps.DeleteFunc(s.sessions, func(id string, _ Session) bool { return id != keep })
	s.mu.Unlock()
}

func (s *SessionStore) SweepExpired() {
	now := time.Now()
	s.mu.Lock()
	maps.DeleteFunc(s.sessions, func(_ string, sess Session) bool {
		return now.After(sess.ExpiresAt)
	})
	s.mu.Unlock()
}

func sessionFromRequest(r *http.Request) string {
	c, err := r.Cookie("monbooru_session")
	if err != nil {
		return ""
	}
	return c.Value
}

func sessionFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionContextKey).(string)
	return v
}

func (s *Server) sessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The API checks its own bearer tokens.
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			next.ServeHTTP(w, r)
			return
		}

		// An "anon" session, so the CSRF check has one to validate.
		if isPublicPath(r.URL.Path) || isStaticPath(r.URL.Path) {
			ctx := context.WithValue(r.Context(), sessionContextKey, "anon")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		if !s.authEnabled() {
			ctx := context.WithValue(r.Context(), sessionContextKey, "anon")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		sessID := sessionFromRequest(r)
		_, ok := s.sessions.GetSession(sessID)
		if !ok {
			// No HX-Redirect: a poll would take the page, and whatever was
			// typed on it, to the login.
			if isHTMXRequest(r) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			target := "/login"
			if r.Method == http.MethodGet && r.URL.Path != "/" {
				target += "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}

		ctx := context.WithValue(r.Context(), sessionContextKey, sessID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// The login page and the wizard load the theme files, browsers fetch the
// manifest without credentials, and the single-instance check and the
// container healthcheck read /health's body.
func isPublicPath(path string) bool {
	switch path {
	case "/login", "/health", "/manifest.json", "/theme.css", "/theme.logo", "/theme.favicon":
		return true
	}
	return false
}

func isStaticPath(path string) bool { return len(path) >= 8 && path[:8] == "/static/" }

func isHTMXRequest(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// X-Forwarded-For is trusted only from a loopback peer, a same-host
// proxy, and only the entry that proxy appended: the ones before it are
// whatever the client sent.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if vals := r.Header.Values("X-Forwarded-For"); len(vals) > 0 {
			xff := vals[len(vals)-1]
			if last := strings.TrimSpace(xff[strings.LastIndex(xff, ",")+1:]); last != "" {
				return last
			}
		}
	}
	return host
}

// Browsers read "//host", a backslash, and a path whose tab or newline they
// strip into "//", as another host.
func localPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.Contains(p, `\`) {
		return false
	}
	return !strings.ContainsFunc(p, func(c rune) bool { return c < 0x20 || c == 0x7f })
}

// sameOriginReferer keeps a form's 303 from following a forged Referer
// off-site or into a javascript: URL.
func sameOriginReferer(r *http.Request) string {
	ref := r.Referer()
	if ref == "" {
		return "/"
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "/"
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return "/"
	}
	if u.Host != "" && u.Host != r.Host {
		return "/"
	}
	return ref
}

type loginRateLimiter struct {
	mu       sync.Mutex
	failures map[string]loginAttempt
}

type loginAttempt struct {
	count    int
	lastFail time.Time
}

func newLoginRateLimiter() *loginRateLimiter {
	return &loginRateLimiter{failures: map[string]loginAttempt{}}
}

func (l *loginRateLimiter) check(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.failures[ip]
	if !ok {
		return true
	}
	// 1s doubling to a 30s cap; the shift is clamped at 0 because a
	// negative shift panics.
	shift := min(max(a.count-1, 0), 5)
	delay := time.Duration(1<<shift) * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return time.Since(a.lastFail) >= delay
}

func (l *loginRateLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.failures[ip]
	a.count++
	a.lastFail = time.Now()
	l.failures[ip] = a
}

func (l *loginRateLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}

func (l *loginRateLimiter) sweep() {
	cutoff := time.Now().Add(-5 * time.Minute)
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, a := range l.failures {
		if a.lastFail.Before(cutoff) {
			delete(l.failures, ip)
		}
	}
}
