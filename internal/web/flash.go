package web

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
)

func flashErr(w http.ResponseWriter, r *http.Request, msg string) {
	if isHTMXRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `<div class="flash flash-err">%s</div>`, html.EscapeString(msg))
		return
	}
	http.Error(w, msg, http.StatusBadRequest)
}

// flashStatus is for HTMX-only callers: it does not branch on the request mode.
func flashStatus(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	writeInlineFlash(w, "err", msg)
}

func setDialogSavedTrigger(w http.ResponseWriter, event, dialogID string) {
	payload, _ := json.Marshal(map[string]any{event: map[string]any{"dialog": dialogID}})
	w.Header().Set("HX-Trigger", string(payload))
}

func writeOOBSummaryFlash(w http.ResponseWriter, spanID, summary, flashID, msg string) {
	_, _ = fmt.Fprintf(w,
		`<span id="%s" hx-swap-oob="true">%s</span><div id="%s" hx-swap-oob="true"><div class="flash flash-ok">%s</div></div>`,
		html.EscapeString(spanID), html.EscapeString(summary), html.EscapeString(flashID), html.EscapeString(msg))
}
