package web

import (
	"cmp"
	"fmt"
	"html"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type fetchStatusEntry struct {
	State  string
	Msg    string
	Hashes string
	At     time.Time
}

const (
	// A batch fetch no page polls would otherwise grow the map without bound.
	fetchStatusTTL   = 10 * time.Minute
	fetchPollMax     = 30
	fetchPollDelayMs = 2000
)

func fetchStatusKey(gallery string, id int64) string {
	return gallery + "\x00" + strconv.FormatInt(id, 10)
}

type fetchStatusStore struct {
	mu sync.Mutex
	m  map[string]fetchStatusEntry
}

func newFetchStatusStore() *fetchStatusStore {
	return &fetchStatusStore{m: map[string]fetchStatusEntry{}}
}

// A terminal report inherits Hashes, which monloader's callback does not
// know; a fresh pending drops them.
func (f *fetchStatusStore) record(gallery string, id int64, state, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	f.pruneLocked(now)
	key := fetchStatusKey(gallery, id)
	entry := fetchStatusEntry{State: state, Msg: msg, At: now}
	if prev, ok := f.m[key]; ok && state != "pending" {
		entry.Hashes = prev.Hashes
	}
	f.m[key] = entry
}

// Callers set it before the enqueue, or a fast PTR callback could be
// overwritten back to pending.
func (f *fetchStatusStore) recordLookup(gallery string, id int64, hashes string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	f.pruneLocked(now)
	f.m[fetchStatusKey(gallery, id)] = fetchStatusEntry{State: "pending", At: now, Hashes: hashes}
}

// The writers prune too, but once the last fetch lands nothing writes again.
func (f *fetchStatusStore) prune() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked(time.Now())
}

func (f *fetchStatusStore) pruneLocked(now time.Time) {
	if f.m == nil {
		f.m = map[string]fetchStatusEntry{}
		return
	}
	maps.DeleteFunc(f.m, func(_ string, e fetchStatusEntry) bool {
		return now.Sub(e.At) > fetchStatusTTL
	})
}

func (f *fetchStatusStore) load(gallery string, id int64) (fetchStatusEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[fetchStatusKey(gallery, id)]
	return e, ok
}

func (f *fetchStatusStore) clear(gallery string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, fetchStatusKey(gallery, id))
}

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
	e, ok := s.fetchStatus.load(s.activeGallery(), id)
	if !ok {
		// Nothing in flight: an empty body stops the poll and clears the slot.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}
	switch e.State {
	case "pending":
		if n >= fetchPollMax {
			writeFetchOutcome(w, "warn", html.EscapeString("Still fetching from monloader; reload to check for new tags."))
			return
		}
		writeFetchPending(w, id, n)
	case "ok":
		// The flash survives HX-Refresh through the stash-and-show bridge.
		s.fetchStatus.clear(s.activeGallery(), id)
		msg := e.Msg
		msg = cmp.Or(msg, "Fetched tags from the source.")
		setFlashHeader(w, msg, "ok", nil)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
	case "hash_not_found":
		// Common and expected, since an altered copy never matches, so it
		// reads as a result, not an error.
		s.fetchStatus.clear(s.activeGallery(), id)
		writeFetchOutcome(w, "warn", lookupMissBody(e.Msg, e.Hashes))
	case "canceled":
		s.fetchStatus.clear(s.activeGallery(), id)
		writeFetchOutcome(w, "warn", "monloader dropped this job before it ran; nothing was looked up.")
	case "already_exists":
		s.fetchStatus.clear(s.activeGallery(), id)
		writeFetchOutcome(w, "warn", alreadyExistsBody(e.Msg))
	default:
		s.fetchStatus.clear(s.activeGallery(), id)
		writeFetchOutcome(w, "err", html.EscapeString(fetchFailureMessage(e.State, e.Msg)))
	}
}

var imageRefRe = regexp.MustCompile(`image (\d+)`)

func alreadyExistsBody(msg string) string {
	return imageRefRe.ReplaceAllString(html.EscapeString(msg), `<a href="/images/$1">image $1</a>`)
}

// Must match how monloader's trail names its PTR backend.
const ptrTrailName = "Public Tag Repository"

func lookupMissBody(trail, hashes string) string {
	var b strings.Builder
	if trail == "" {
		b.WriteString("No source found; no tags found.")
	} else {
		var online []string
		for _, entry := range strings.Split(trail, "; ") {
			if !strings.HasPrefix(entry, ptrTrailName+":") {
				online = append(online, entry)
				continue
			}
			// A PTR match sets no source URL, so the lookup reports a
			// miss even though its tags landed.
			b.WriteString("<div>" + html.EscapeString(entry))
			if strings.HasPrefix(entry, ptrTrailName+": match") {
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
	if !similarityLookupRan(trail) {
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

func similarityLookupRan(trail string) bool {
	for _, entry := range strings.Split(trail, "; ") {
		name, reason, ok := strings.Cut(entry, ": ")
		if !ok {
			continue
		}
		if (name == "iqdb" || name == "saucenao") && !strings.HasPrefix(reason, "skipped") {
			return true
		}
	}
	return false
}

// Keyed on monloader's stable queue error codes.
var fetchFailureMessages = map[string]string{
	"unsupported_url":      "monloader can't fetch this source URL.",
	"network_unreachable":  "monloader couldn't reach the source.",
	"auth_required":        "The source needs a login monloader doesn't have.",
	"blocked":              "The source blocked monloader's fetch.",
	"rate_limited":         "The source is rate-limiting; try again later.",
	"download_failed":      "The source fetch failed on monloader.",
	"monbooru_unreachable": "monloader fetched the source but couldn't apply the tags.",
	"monbooru_rejected":    "monloader fetched the source but couldn't apply the tags.",
	"mapping_failed":       "monloader couldn't read the source's metadata.",
}

func fetchFailureMessage(state, msg string) string {
	return cmp.Or(fetchFailureMessages[state], msg, "The source fetch failed.")
}
