// Package api implements the /api/v1/ REST handlers for monbooru.
package api

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/relations"
)

type Gallery struct {
	gallery.Handle

	RelationsSvc     *relations.Service
	InvalidateCaches func()
	RecordFetch      func(imageID int64, state, message string)
}

func (g Gallery) invalidate() {
	if g.InvalidateCaches != nil {
		g.InvalidateCaches()
	}
}

func (g Gallery) recordFetch(imageID int64, state, message string) {
	if g.RecordFetch != nil {
		g.RecordFetch(imageID, state, message)
	}
}

func relationsOnDelete(svc *relations.Service) func(*sql.Tx, int64) error {
	if svc == nil {
		return nil
	}
	return svc.OnImageDeleteTx
}

// ResolverFunc resolves an empty name to the active gallery.
type ResolverFunc func(name string) (Gallery, bool)

type Handler struct {
	// Must return a snapshot: settings rewrites the live config under a
	// lock this package does not hold.
	cfg      func() *config.Config
	jobs     *jobs.Manager
	resolver ResolverFunc
	version  string
	lock     func() (unlock func())
}

func New(cfg func() *config.Config, jobManager *jobs.Manager, resolver ResolverFunc, version string) *Handler {
	return &Handler{cfg: cfg, jobs: jobManager, resolver: resolver, version: version}
}

// WithGalleryLock hands the upload routes the gallery lock their caller
// holds around every other route, to take once their body is read.
func (h *Handler) WithGalleryLock(lock func() (unlock func())) *Handler {
	h.lock = lock
	return h
}

// A slow push holding the gallery lock while its body arrives would stall
// every request queued behind a gallery switch.
func (h *Handler) bodyThenLock(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isMultipart(r.Header.Get("Content-Type")) {
			// A zero or negative size disables the cap; the 4 KiB slack
			// alone would refuse every push.
			if maxBytes := int64(h.cfg().Gallery.MaxFileSizeMB) * 1024 * 1024; maxBytes > 0 {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes+4096)
			}
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
					apiError(w, http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds max size")
				} else {
					apiError(w, http.StatusBadRequest, "invalid_request", "invalid multipart body")
				}
				return
			}
		}
		if h.lock != nil {
			defer h.lock()()
		}
		next(w, r)
	}
}

func (h *Handler) uploadDestination() (folder, name string) {
	cfg := h.cfg()
	return cfg.Gallery.DefaultUploadFolder, cfg.Gallery.DefaultUploadName
}

func (h *Handler) resolveGallery(w http.ResponseWriter, r *http.Request) (Gallery, bool) {
	name := strings.TrimSpace(r.URL.Query().Get("gallery"))
	name = cmp.Or(name, strings.TrimSpace(r.Header.Get("X-Monbooru-Gallery")))
	g, ok := h.resolver(name)
	if !ok {
		if name == "" {
			apiError(w, http.StatusServiceUnavailable, "api_disabled", "no active gallery")
		} else {
			apiError(w, http.StatusBadRequest, "invalid_gallery", "unknown gallery: "+name)
		}
		return Gallery{}, false
	}
	return g, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return false
	}
	return true
}

func (h *Handler) galleryAndID(w http.ResponseWriter, r *http.Request) (Gallery, int64, bool) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return Gallery{}, 0, false
	}
	id, ok := apiPathInt64(w, r, "id")
	if !ok {
		return Gallery{}, 0, false
	}
	return g, id, true
}

// Without the probe, a tag add on a missing id still creates its tags
// before failing.
func (h *Handler) galleryAndExistingID(w http.ResponseWriter, r *http.Request) (Gallery, int64, bool) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return Gallery{}, 0, false
	}
	if !imageExists(g, id) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return Gallery{}, 0, false
	}
	return g, id, true
}

func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/images", h.auth(h.bodyThenLock(h.createImage)))
	mux.HandleFunc("GET /api/v1/images/search", h.auth(h.searchImages))
	mux.HandleFunc("GET /api/v1/images/{id}", h.auth(h.getImage))
	mux.HandleFunc("PATCH /api/v1/images/{id}", h.auth(h.patchImage))
	mux.HandleFunc("DELETE /api/v1/images/{id}", h.auth(h.deleteImage))
	mux.HandleFunc("GET /api/v1/images/{id}/tags", h.auth(h.listImageTags))
	mux.HandleFunc("POST /api/v1/images/{id}/tags", h.auth(h.addImageTags))
	mux.HandleFunc("DELETE /api/v1/images/{id}/tags", h.auth(h.removeImageTags))
	mux.HandleFunc("POST /api/v1/images/{id}/enrich", h.auth(h.enrichImage))
	mux.HandleFunc("POST /api/v1/images/{id}/fetch-status", h.auth(h.fetchStatusReport))

	mux.HandleFunc("GET /api/v1/images/{id}/file", h.auth(h.serveImageFile))
	mux.HandleFunc("POST /api/v1/images/{id}/file", h.auth(h.bodyThenLock(h.replaceImageFile)))
	mux.HandleFunc("GET /api/v1/images/{id}/thumbnail", h.auth(h.serveThumbnail))
	mux.HandleFunc("GET /api/v1/images/{id}/page/{n}", h.auth(h.serveMangaPage))
	mux.HandleFunc("GET /api/v1/images/{id}/page/{n}/thumb", h.auth(h.serveMangaPageThumb))

	mux.HandleFunc("GET /api/v1/tags", h.auth(h.listTags))
	mux.HandleFunc("POST /api/v1/tags", h.auth(h.createTag))
	mux.HandleFunc("POST /api/v1/tags/aliases", h.auth(h.createAlias))
	mux.HandleFunc("POST /api/v1/tags/merge", h.auth(h.mergeTags))
	mux.HandleFunc("GET /api/v1/tags/{id}", h.auth(h.getTag))
	mux.HandleFunc("PATCH /api/v1/tags/{id}", h.auth(h.patchTag))
	mux.HandleFunc("DELETE /api/v1/tags/{id}", h.auth(h.deleteTag))
	mux.HandleFunc("GET /api/v1/tags/{id}/implications", h.auth(h.listImplications))
	mux.HandleFunc("POST /api/v1/tags/{id}/implications", h.auth(h.addImplication))
	mux.HandleFunc("DELETE /api/v1/tags/{id}/implications/{impliedID}", h.auth(h.removeImplication))

	mux.HandleFunc("GET /api/v1/categories", h.auth(h.listCategories))
	mux.HandleFunc("POST /api/v1/categories", h.auth(h.createCategory))
	mux.HandleFunc("PATCH /api/v1/categories/{id}", h.auth(h.patchCategory))
	mux.HandleFunc("DELETE /api/v1/categories/{id}", h.auth(h.deleteCategory))

	mux.HandleFunc("GET /api/v1/galleries", h.auth(h.listGalleries))

	mux.HandleFunc("GET /api/v1/images/{id}/relations", h.auth(h.relationsForImage))
	mux.HandleFunc("POST /api/v1/relations", h.auth(h.addRelation))
	mux.HandleFunc("DELETE /api/v1/relations", h.auth(h.removeRelation))

	mux.HandleFunc("GET /api/v1/openapi.json", h.openAPIJSON)
	mux.HandleFunc("GET /api/v1/docs", h.openAPIDocs)

	mux.HandleFunc("OPTIONS /api/v1/", h.preflight)

	mux.HandleFunc("GET /api/v1/", h.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/" {
			apiError(w, http.StatusNotFound, "not_found", "endpoint not found: "+r.URL.Path)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"api":     "monbooru",
			"version": h.version,
			"docs":    "/api/v1/docs",
			"openapi": "/api/v1/openapi.json",
		})
	}))
}

func requireID(w http.ResponseWriter, v int64, name string) bool {
	if v == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", name+" required")
		return false
	}
	return true
}

func SetCORS(w http.ResponseWriter, r *http.Request, cfg *config.Config) bool {
	w.Header().Set("Vary", "Origin")
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if !corsAllowed(cfg, r, origin) {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	// Content-Disposition is not CORS-safelisted; without this a browser
	// client loses the download's filename.
	w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")
	return true
}

// The arrival address counts alongside base_url: a base_url left at
// "localhost" would otherwise refuse the page monbooru itself just served.
func corsAllowed(cfg *config.Config, r *http.Request, origin string) bool {
	if self := requestOrigin(cfg, r); self != "" && origin == self {
		return true
	}
	// Origin never ends in a slash; base_url may.
	if origin == strings.TrimRight(cfg.Server.BaseURL, "/") {
		return true
	}
	return slices.ContainsFunc(cfg.Server.CORSOrigins, func(allowed string) bool {
		return allowed == "*" || allowed == origin
	})
}

// Empty unless Host is a literal address: Host is client-supplied, so a
// rebound or forged name would vouch for itself. The scheme comes from
// base_url, which a TLS-terminating proxy hides from the listener.
func requestOrigin(cfg *config.Config, r *http.Request) string {
	name := r.Host
	if h, _, err := net.SplitHostPort(name); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	if name != "localhost" && net.ParseIP(name) == nil {
		return ""
	}
	scheme := "http"
	if strings.HasPrefix(cfg.Server.BaseURL, "https://") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// Outside auth: a preflight never carries credentials.
func (h *Handler) preflight(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg()
	if !SetCORS(w, r, cfg) {
		apiError(w, http.StatusForbidden, "forbidden", "CORS: origin not allowed")
		return
	}
	if r.Header.Get("Origin") != "" {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Monbooru-Gallery")
		w.Header().Set("Access-Control-Max-Age", "600")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := h.cfg()
		if !SetCORS(w, r, cfg) {
			apiError(w, http.StatusForbidden, "forbidden", "CORS: origin not allowed")
			return
		}

		if len(cfg.Auth.Tokens) == 0 {
			apiError(w, http.StatusServiceUnavailable, "api_disabled",
				"API is disabled: generate an API token in Settings to enable it")
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			apiError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid authorization header")
			return
		}
		tok := cfg.FindTokenByHash(config.HashToken(auth[len(prefix):]))
		if tok == nil {
			apiError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}
		scope := scopeForMethod(r.Method)
		if !tok.HasScope(scope) {
			apiError(w, http.StatusForbidden, "insufficient_scope", "token lacks the "+scope+" scope")
			return
		}

		next(w, r)
	}
}

func scopeForMethod(method string) string {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		return config.ScopeWrite
	case http.MethodDelete:
		return config.ScopeDelete
	default:
		return config.ScopeRead
	}
}

func apiPathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid "+name)
		return 0, false
	}
	return v, true
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

func serverError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
	return true
}

func badRequest(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
	return true
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writePage(w http.ResponseWriter, page, limit, total int, results any) {
	WriteJSON(w, http.StatusOK, map[string]any{
		"page":    page,
		"limit":   limit,
		"total":   total,
		"results": results,
	})
}

func parsePage(r *http.Request, defaultLimit, maxLimit int) (offset, limit int) {
	page := 1
	limit = defaultLimit
	q := r.URL.Query()
	if p := q.Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}
	l := q.Get("limit")
	l = cmp.Or(l, q.Get("page_size"))
	if l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = min(n, maxLimit)
		}
	}
	// Past this the offset wraps negative and serves the first page.
	page = min(page, math.MaxInt/limit)
	return (page - 1) * limit, limit
}
