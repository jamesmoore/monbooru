package web

import (
	"cmp"
	"net/http"

	"github.com/monbooru/monbooru/internal/shared/logx"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		s.renderTemplate(w, "login.html", s.loginPageData(map[string]any{
			"Error":        "Password authentication is disabled. Enable it from Settings → Authentication.",
			"AuthDisabled": true,
		}))
		return
	}
	s.renderTemplate(w, "login.html", s.loginPageData(map[string]any{"Next": loginNext(r.URL.Query().Get("next"))}))
}

func loginNext(p string) string {
	if localPath(p) {
		return p
	}
	return ""
}

func (s *Server) loginPageData(extra map[string]any) map[string]any {
	return s.standalonePageData("Login - "+s.booruName(), extra)
}

// Carries every field partials/head.html reads: these pages skip s.base().
func (s *Server) standalonePageData(title string, extra map[string]any) map[string]any {
	theme := s.activeTheme()
	data := map[string]any{
		"Title":        title,
		"CSRFToken":    s.sessions.CSRFToken("anon"),
		"BooruName":    s.booruName(),
		"BooruFavicon": themeFaviconURL(theme),
		"Theme":        theme.Path != "",
	}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	next := loginNext(r.FormValue("next"))
	ip := clientIP(r)
	if !s.loginRL.check(ip) {
		logx.Warnf("login rate-limited from %s", ip)
		s.renderTemplate(w, "login.html", s.loginPageData(map[string]any{
			"Error": "Too many attempts. Please wait before trying again.",
			"Next":  next,
		}))
		return
	}

	password := r.FormValue("password")
	if err := bcrypt.CompareHashAndPassword(
		[]byte(s.passwordHash()), []byte(password),
	); err != nil {
		s.loginRL.recordFailure(ip)
		logx.Warnf("login failed from %s", ip)
		s.renderTemplate(w, "login.html", s.loginPageData(map[string]any{
			"Error": "Invalid password",
			"Next":  next,
		}))
		return
	}
	s.loginRL.recordSuccess(ip)
	logx.Infof("login success from %s", ip)

	s.setSessionCookie(w, r, s.sessions.Create(s.sessionLifetimeDays()))
	http.Redirect(w, r, cmp.Or(next, "/"), http.StatusSeeOther)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, sessID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "monbooru_session",
		Value:    sessID,
		Path:     "/",
		MaxAge:   s.sessionLifetimeDays() * 86400,
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) logoutPost(w http.ResponseWriter, r *http.Request) {
	sessID := sessionFromContext(r.Context())
	s.sessions.Delete(sessID)
	http.SetCookie(w, &http.Cookie{
		Name:     "monbooru_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) renderAuthPasswordOOB(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "partials/auth_password_section.html", map[string]any{
		"AuthEnabled": s.authEnabled(),
		"HasPassword": s.passwordHash() != "",
		"OOB":         true,
	})
}
