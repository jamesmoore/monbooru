package web

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/tagger"
)

func pathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return v, true
}

func pathTaggerName(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := strings.TrimSpace(r.PathValue("name"))
	if err := tagger.ValidateTaggerName(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	return v, true
}

func formInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	// Status 200 so HTMX swaps the flash into the dialog (it drops 4xx swaps).
	writeFieldFlash := func(verb string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `<div class="flash flash-err">%s %s.</div>`, verb, html.EscapeString(name))
	}
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		writeFieldFlash("Missing")
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeFieldFlash("Invalid")
		return 0, false
	}
	return v, true
}

// The id goes first, so a request wrong about both answers 404.
func idAndForm(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return 0, false
	}
	if !parseFormOK(w, r) {
		return 0, false
	}
	return id, true
}

func taggerNameAndForm(w http.ResponseWriter, r *http.Request) (string, bool) {
	name, ok := pathTaggerName(w, r)
	if !ok {
		return "", false
	}
	if !parseFormOK(w, r) {
		return "", false
	}
	return name, true
}
