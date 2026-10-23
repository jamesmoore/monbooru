package web

import (
	"encoding/json"
	"net/http"
)

// flashStatus is for HTMX-only callers: it does not branch on the request mode.
// htmx swaps a 2xx and main.js a 409; any other refusal is a 200 flash (hxErr) or, never with a 409, the flash header (flashHeaderErr).
func flashStatus(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	writeInlineFlash(w, "err", msg)
}

func flashHeaderErr(w http.ResponseWriter, code int, msg string) {
	setFlashHeader(w, msg, "err", nil)
	flashStatus(w, code, msg)
}

func hxErr(w http.ResponseWriter, r *http.Request, htmxMsg, plainMsg string, code int) {
	if isHTMXRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		flashStatus(w, http.StatusOK, htmxMsg)
		return
	}
	http.Error(w, plainMsg, code)
}

func externalErr(w http.ResponseWriter, r *http.Request, msg string, code int) {
	hxErr(w, r, msg, msg, code)
}

func setDialogSavedTrigger(w http.ResponseWriter, event, dialogID string) {
	payload, _ := json.Marshal(map[string]any{event: map[string]any{"dialog": dialogID}})
	w.Header().Set("HX-Trigger", string(payload))
}
