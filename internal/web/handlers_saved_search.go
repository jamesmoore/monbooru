package web

import (
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/logx"
)

func (s *Server) deleteSavedSearch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.db().Write.Exec(`DELETE FROM saved_searches WHERE id = ?`, id); err != nil {
		logx.Warnf("delete saved search %d: %v", id, err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	s.active().InvalidateCaches()
	// 200 + empty body - HTMX outerHTML swap removes the element.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) createSavedSearch(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	query := strings.TrimSpace(r.FormValue("query"))
	if name == "" || query == "" {
		externalErr(w, r, "Name and query required.", http.StatusBadRequest)
		return
	}
	sortStr := strings.TrimSpace(r.FormValue("sort"))
	orderStr := strings.TrimSpace(r.FormValue("order"))
	seedStr := strings.TrimSpace(r.FormValue("seed"))
	// Plain INSERT: a name clash must error, not overwrite the saved search.
	if _, err := s.db().Write.Exec(
		`INSERT INTO saved_searches (name, query, sort, sort_order, seed) VALUES (?, ?, ?, ?, ?)`,
		name, query, sortStr, orderStr, seedStr,
	); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "UNIQUE") {
			msg = "A saved search named " + name + " already exists. Delete it first or pick another name."
		}
		externalErr(w, r, msg, http.StatusBadRequest)
		return
	}
	s.active().InvalidateCaches()
	if isHTMXRequest(r) {
		writeInlineFlash(w, "ok", "Saved.")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
