package web

import (
	"cmp"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/monloader"
)

const (
	fetchPollMax     = 30
	fetchPollDelayMs = 2000
)

func writeFetchPending(w http.ResponseWriter, id, n int64) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w,
		`<span class="fetch-status-msg monloader-accent" hx-get="/internal/images/%d/fetch-status?n=%d" hx-trigger="load delay:%dms" hx-swap="outerHTML">Fetching data via monloader...</span>`,
		id, n+1, fetchPollDelayMs)
}

func respondFetchPending(w http.ResponseWriter, r *http.Request, id int64) {
	if isHTMXRequest(r) {
		writeFetchPending(w, id, 0)
		return
	}
	http.Redirect(w, r, "/images/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// Out of band with an empty main body, so the pill clears wherever it was
// placed. body is raw HTML: the caller escapes it.
func writeFetchOutcome(w http.ResponseWriter, kind, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<div id="fetch-status" class="fetch-status" hx-swap-oob="true"><div class="flash flash-` + kind + `">` + body + `</div></div>`))
}

func (s *Server) fetchStatusHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	n, _ := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
	e, ok := s.ml.Fetches.Load(s.activeGallery(), id)
	if !ok {
		// Nothing in flight: an empty body stops the poll and clears the slot.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}
	switch e.State {
	case monloader.StatePending:
		if n >= fetchPollMax {
			writeFetchOutcome(w, "warn", html.EscapeString("Still fetching from monloader; reload to check for new tags."))
			return
		}
		writeFetchPending(w, id, n)
	case monloader.StateOK:
		// The flash survives HX-Refresh through the stash-and-show bridge.
		s.ml.Fetches.Clear(s.activeGallery(), id)
		msg := cmp.Or(e.Msg, "Fetched tags from the source.")
		setFlashHeader(w, msg, "ok", nil)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
	case monloader.StateHashNotFound:
		// Common and expected, since an altered copy never matches, so it
		// reads as a result, not an error.
		s.ml.Fetches.Clear(s.activeGallery(), id)
		writeFetchOutcome(w, "warn", lookupMissBody(e.Msg, e.Hashes))
	case monloader.StateCanceled:
		s.ml.Fetches.Clear(s.activeGallery(), id)
		writeFetchOutcome(w, "warn", "monloader dropped this job before it ran; nothing was looked up.")
	case monloader.StateAlreadyExists:
		s.ml.Fetches.Clear(s.activeGallery(), id)
		writeFetchOutcome(w, "warn", alreadyExistsBody(e.Msg))
	default:
		s.ml.Fetches.Clear(s.activeGallery(), id)
		writeFetchOutcome(w, "err", html.EscapeString(fetchFailureMessage(e.State, e.Msg)))
	}
}

var imageRefRe = regexp.MustCompile(`image (\d+)`)

func alreadyExistsBody(msg string) string {
	return imageRefRe.ReplaceAllString(html.EscapeString(msg), `<a href="/images/$1">image $1</a>`)
}

func lookupMissBody(trail, hashes string) string {
	var b strings.Builder
	if trail == "" {
		b.WriteString("No source found; no tags found.")
	} else {
		ptr, online := monloader.SplitTrail(trail)
		for _, entry := range ptr {
			// A PTR match sets no source URL, so the lookup reports a
			// miss even though its tags landed.
			b.WriteString("<div>" + html.EscapeString(entry))
			if strings.HasPrefix(entry, monloader.PTRTrailName+": match") {
				b.WriteString(" (reload the page to see them)")
			}
			b.WriteString("</div>")
		}
		if len(online) > 0 {
			b.WriteString("No online source found:<ul class=\"lookup-miss-trail\">")
			for _, entry := range online {
				b.WriteString("<li>" + linkifyTrailEntry(entry) + "</li>")
			}
			b.WriteString("</ul>")
		}
	}
	if hashes != "" {
		b.WriteString(`<span class="field-hint">Searched ` + html.EscapeString(hashes) + `</span>`)
	}
	if !monloader.SimilarityRan(trail) {
		b.WriteString(`<div class="field-hint">A miss can happen when the file was compressed or re-encoded, since its hash no longer matches the original. Set up a similarity lookup service in monloader so it can still find such copies.</div>`)
	}
	return b.String()
}

func linkifyTrailEntry(entry string) string {
	var b strings.Builder
	for {
		i := strings.Index(entry, "http://")
		if j := strings.Index(entry, "https://"); j != -1 && (i == -1 || j < i) {
			i = j
		}
		if i == -1 {
			b.WriteString(html.EscapeString(entry))
			return b.String()
		}
		b.WriteString(html.EscapeString(entry[:i]))
		rest := entry[i:]
		end := strings.IndexAny(rest, " ,")
		if end == -1 {
			end = len(rest)
		}
		raw := rest[:end]
		label := raw
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			label = strings.TrimPrefix(u.Host, "www.")
		}
		b.WriteString(`<a href="` + html.EscapeString(raw) + `" target="_blank" rel="noopener">` + html.EscapeString(label) + `</a>`)
		entry = rest[end:]
	}
}

// Keyed on monloader's stable queue error codes.
var fetchFailureMessages = map[string]string{
	monloader.CodeUnsupportedURL:      "monloader can't fetch this source URL.",
	monloader.CodeNetworkUnreachable:  "monloader couldn't reach the source.",
	monloader.CodeAuthRequired:        "The source needs a login monloader doesn't have.",
	monloader.CodeBlocked:             "The source blocked monloader's fetch.",
	monloader.CodeRateLimited:         "The source is rate-limiting; try again later.",
	monloader.CodeDownloadFailed:      "The source fetch failed on monloader.",
	monloader.CodeMonbooruUnreachable: "monloader fetched the source but couldn't apply the tags.",
	monloader.CodeMonbooruRejected:    "monloader fetched the source but couldn't apply the tags.",
	monloader.CodeMappingFailed:       "monloader couldn't read the source's metadata.",
}

func fetchFailureMessage(state, msg string) string {
	return cmp.Or(fetchFailureMessages[state], msg, "The source fetch failed.")
}
