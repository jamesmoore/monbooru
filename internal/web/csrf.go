package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"log"
	"mime"
	"net/http"
	"strings"
)

func mustRandBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("FATAL: failed to generate CSRF secret: %v", err)
	}
	return b
}

func (s *Server) csrfToken(sessionID string) string {
	mac := hmac.New(sha256.New, s.csrfSecret)
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) validateCSRF(sessionID, token string) bool {
	expected := s.csrfToken(sessionID)
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func parseFormOK(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		flashErr(w, r, "Bad form data: "+err.Error())
		return false
	}
	return true
}

func requiredFormFlash(w http.ResponseWriter, r *http.Request, field, msg string) (string, bool) {
	v := strings.TrimSpace(r.FormValue(field))
	if v == "" {
		flashStatus(w, http.StatusBadRequest, msg)
		return "", false
	}
	return v, true
}

func requiredFormExternal(w http.ResponseWriter, r *http.Request, field, msg string) (string, bool) {
	v := strings.TrimSpace(r.FormValue(field))
	if v == "" {
		externalErr(w, r, msg, http.StatusBadRequest)
		return "", false
	}
	return v, true
}

// A page rendered before a restart, or under a session since replaced,
// holds a token that no longer validates; a browser that says the request
// comes from this origin is no forgery either way.
var sameOrigin http.CrossOriginProtection

// Without either header nothing vouches for the request (a script), so it
// still needs the token.
func browserSameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "" && r.Header.Get("Origin") == "" {
		return false
	}
	return sameOrigin.Check(r) == nil
}

// /api/v1/ is exempt: its bearer token is the CSRF mitigation.
func (s *Server) cSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		if len(r.URL.Path) >= 8 && r.URL.Path[:8] == "/api/v1/" {
			next.ServeHTTP(w, r)
			return
		}

		// A mounted plugin's forms carry no monbooru token, and with login
		// off no session gates the mount either.
		if strings.HasPrefix(r.URL.Path, pluginMountPrefix) {
			if sameOrigin.Check(r) != nil {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		sessID := sessionFromContext(r.Context())

		// Header first: FormValue parses the body, and on multipart it
		// drains up to 32 MiB past any handler's MaxBytesReader, so
		// multipart callers must send the header.
		token := r.Header.Get("X-CSRF-Token")
		if token == "" && !isMultipart(r) {
			token = r.FormValue("_csrf")
		}

		if !s.validateCSRF(sessID, token) && !browserSameOrigin(r) {
			http.Error(w, "This page's form token is no longer valid. Copy anything unsaved, then reload the page.", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func isMultipart(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return len(mt) > 10 && mt[:10] == "multipart/"
}
