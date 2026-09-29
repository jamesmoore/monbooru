package web

import (
	"cmp"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tags"
	"github.com/monbooru/monbooru/internal/upgrade"
)

type originTagGroup struct {
	Origin string
	Stale  bool
	Tags   []models.Tag
}

type originImplicationGroup struct {
	Origin       string
	Stale        bool
	Implications []models.Implication
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"seq": func(start, end int) []int {
			r := make([]int, 0, end-start+1)
			for i := start; i <= end; i++ {
				r = append(r, i)
			}
			return r
		},
		"add": func(a, b int) int { return a + b },
		// template.CSS to pass the style-attribute sanitizer;
		// categoryColor admits only a validated colour.
		"catColor":   func(color string) template.CSS { return template.CSS(categoryColor(color)) },
		"catDefault": tags.DefaultCategoryColor,
		// template.URL, or html/template would re-escape each % and
		// double-encode the link.
		"urlQ": func(s string) template.URL {
			return template.URL(url.QueryEscape(s))
		},
		"qval": search.QuoteValue,
		"sub":  func(a, b int) int { return a - b },
		"pct":  func(f float64) int { return int(math.Round(f * 100)) },
		"list": func(vs ...any) []any { return vs },
		"dict": func(pairs ...any) map[string]any {
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i+1 < len(pairs); i += 2 {
				k, _ := pairs[i].(string)
				m[k] = pairs[i+1]
			}
			return m
		},
		"groupByCategory": func(tagList []models.Tag) []tagGroup {
			return groupOrdered(tagList, nil,
				func(t models.Tag) string { return t.CategoryName },
				func(t models.Tag) *tagGroup { return &tagGroup{Name: t.CategoryName, Color: t.CategoryColor} },
				func(g *tagGroup, t models.Tag) { g.Tags = append(g.Tags, t) })
		},
		"deref":    derefOr[int],
		"deref64":  derefOr[int64],
		"deref64f": derefOr[float64],
		"elideHash": func(h string) string {
			if len(h) <= 19 {
				return h
			}
			return h[:8] + "..." + h[len(h)-8:]
		},
		"phashHex": func(p *int64) string {
			if p == nil {
				return ""
			}
			return fmt.Sprintf("%016x", uint64(*p))
		},
		"upgradable":     upgrade.Eligible,
		"upgradeCompare": upgradeCompare,
		"isPTRSite": func(site string) bool {
			return strings.EqualFold(strings.TrimSpace(site), "ptr")
		},
		"groupTagsByOrigin": func(list []models.Tag) []originTagGroup {
			return groupByOriginStale(list,
				func(t models.Tag) (string, bool) { return t.Origin, t.Stale },
				func(t models.Tag) originTagGroup { return originTagGroup{Origin: t.Origin, Stale: t.Stale} },
				func(g *originTagGroup, t models.Tag) { g.Tags = append(g.Tags, t) })
		},
		"groupImplicationsByOrigin": func(list []models.Implication) []originImplicationGroup {
			return groupByOriginStale(list,
				func(im models.Implication) (string, bool) { return im.Origin, im.Stale },
				func(im models.Implication) originImplicationGroup {
					return originImplicationGroup{Origin: im.Origin, Stale: im.Stale}
				},
				func(g *originImplicationGroup, im models.Implication) { g.Implications = append(g.Implications, im) })
		},
		"originLabel": func(kinds map[string]string, origin string) string {
			switch {
			case origin == "":
				return "an unrecorded source"
			case kinds[origin] == "user":
				return "the user"
			case kinds[origin] == "ptr" || strings.EqualFold(origin, "ptr"):
				return "the Public Tag Repository"
			default:
				return origin
			}
		},
		"cancelTitle": cancelTitle,
		"humanBytes":  humanBytesFmt,
		"localTime": func(t time.Time) string {
			return t.In(time.Local).Format("2006-01-02 15:04:05")
		},
		"localTimePtr": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.In(time.Local).Format("2006-01-02 15:04:05")
		},
		"localDate":          localDay,
		"monloaderOffReason": monloaderOffReason,
		"lookupResultLabel": func(result string) string {
			if result == "hit" {
				return "tags applied"
			}
			return "no match"
		},
		"browseSortLabel": browseSortLabel,
		"isLongValue": func(s string) bool {
			return len(s) > 200 || strings.ContainsAny(s, "\n\r")
		},
		"schedDuration": func(d time.Duration) string {
			if d >= time.Second {
				return d.Round(time.Second).String()
			}
			return d.Round(time.Millisecond).String()
		},
		"minusDuration": func(a, b time.Duration) time.Duration {
			return a - b
		},
		"int64Duration": func(d time.Duration) int64 {
			return int64(d)
		},
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return one
			}
			return many
		},
		"abbrevCount": abbrevCount,
		// Always quoted: a term may carry spaces the parser would split on.
		"comfySearch": func(term string) string {
			if term == "" {
				return ""
			}
			return `comfyui:"` + search.QuoteValue(term) + `"`
		},
		"comfyRefTarget": func(s string) string {
			return strings.TrimPrefix(s, "→ ")
		},
		"hasPrefix":     strings.HasPrefix,
		"providerLabel": providerDisplayLabel,
		"urlDomain": func(s string) string {
			u, err := url.Parse(s)
			if err != nil || u.Host == "" {
				return s
			}
			return strings.TrimPrefix(u.Host, "www.")
		},
		"truncate": truncateRunes,
		"hasFavFilter": func(query string) bool {
			for _, tok := range strings.Fields(query) {
				if strings.EqualFold(tok, "fav:true") {
					return true
				}
			}
			return false
		},
		"pageLoadMs": func(t time.Time) int64 {
			if t.IsZero() {
				return 0
			}
			return time.Since(t).Milliseconds()
		},
	}
}

func localDay(t time.Time) string { return t.In(time.Local).Format("2006-01-02") }

// Only job types whose worker observes ctx.Done() belong here: a missing
// entry renders no cancel button.
var cancelTitles = map[string]string{
	"autotag":         "Stop auto-tagging",
	"sync":            "Stop syncing",
	"delete":          "Stop deleting",
	"re-extract":      "Stop re-extraction",
	"rebuild-thumbs":  "Stop thumbnail rebuild",
	"prune-thumbs":    "Stop thumbnail prune",
	"hashes":          "Stop hash backfill",
	"meta-tags":       "Stop the meta tag pass",
	"index-workflows": "Stop workflow indexing",
	"relations":       "Stop find-pairs",
	"lookup":          "Stop the lookup",
	"move":            "Stop moving",
	"tag":             "Stop tagging",
	"transfer":        "Stop the transfer",
	"check":           "Stop the check",
}

func cancelTitle(jobType string) string { return cancelTitles[jobType] }

var runningJobNames = map[string]string{
	"autotag":         "Auto-tagging",
	"sync":            "A gallery sync",
	"delete":          "A delete",
	"re-extract":      "A re-extraction",
	"rebuild-thumbs":  "A thumbnail rebuild",
	"prune-thumbs":    "A thumbnail prune",
	"prune-dirs":      "A folder prune",
	"hashes":          "A hash backfill",
	"meta-tags":       "A meta tag pass",
	"index-workflows": "A workflow indexing run",
	"relations":       "A find-pairs run",
	"lookup":          "A lookup",
	"move":            "A move",
	"tag":             "A tagging job",
	"transfer":        "A transfer",
	"vacuum":          "A vacuum",
	"free-memory":     "A memory reclaim",
	"fold":            "A fold",
	"check":           "A check",
}

func runningJobName(jobType string) string { return cmp.Or(runningJobNames[jobType], "A job") }

var browseSortLabels = map[string]string{
	"recent":         "Recent",
	"size":           "Size",
	"original_added": "Original added",
	"length":         "Length",
	"newest_member":  "Newest member",
}

func browseSortLabel(s string) string { return cmp.Or(browseSortLabels[s], s) }

var monloaderOffReasons = map[string]string{
	"paused":   "monloader is paused",
	"rejected": "monloader rejected the token",
}

func monloaderOffReason(conn string) string {
	return cmp.Or(monloaderOffReasons[conn], "monloader is not responding")
}

// At most four glyphs; the unit promotes at 999500 because anything
// higher rounds to "1000k".
func abbrevCount(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 999500:
		return abbrevUnit(n, 1000, "k")
	default:
		return abbrevUnit(n, 1000000, "M")
	}
}

func abbrevUnit(n, div int, suffix string) string {
	if tenths := (n*10 + div/2) / div; tenths < 100 {
		return fmt.Sprintf("%d.%d%s", tenths/10, tenths%10, suffix)
	}
	return fmt.Sprintf("%d%s", (n+div/2)/div, suffix)
}

func groupByOriginStale[T, G any](items []T, key func(T) (string, bool), newGroup func(T) G, add func(*G, T)) []G {
	idx := map[string]int{}
	var groups []G
	var isStale []bool
	for _, it := range items {
		origin, stale := key(it)
		k := origin
		if stale {
			k += "\x00stale"
		}
		i, ok := idx[k]
		if !ok {
			i = len(groups)
			idx[k] = i
			groups = append(groups, newGroup(it))
			isStale = append(isStale, stale)
		}
		add(&groups[i], it)
	}
	out := make([]G, 0, len(groups))
	for _, want := range []bool{false, true} {
		for i, g := range groups {
			if isStale[i] == want {
				out = append(out, g)
			}
		}
	}
	return out
}

// The result carries its own separator: a bare space when the post
// published nothing.
func upgradeCompare(s models.ImageSource, img models.Image) string {
	post := fileFacts(s.PostWidth, s.PostHeight, s.PostExt, s.PostSize)
	if post == "" {
		return " "
	}
	label := s.Site
	if label == "" {
		label = "the post"
	}
	pad := max(len("yours"), len(label)) + 2
	return fmt.Sprintf("\n\n%-*s%s\n%-*s%s\n\n",
		pad, "yours", fileFacts(derefOr(img.Width), derefOr(img.Height), strings.ToLower(img.FileType), img.FileSize),
		pad, label, post)
}

func fileFacts(w, h int, ext string, size int64) string {
	var parts []string
	if w > 0 && h > 0 {
		parts = append(parts, fmt.Sprintf("%dx%d", w, h))
	}
	if ext != "" {
		parts = append(parts, ext)
	}
	line := strings.Join(parts, " ")
	if size > 0 {
		if line != "" {
			line += " - "
		}
		line += humanBytesFmt(size)
	}
	return line
}

func derefOr[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// Keyed on the colour, not the category, so a theme can remap the shipped
// palette while an operator's own colour falls through.
func categoryColor(color string) string {
	if !tags.IsValidCategoryColor(color) {
		return tags.SafeCategoryColor(color)
	}
	return "var(--cat-" + strings.ToLower(strings.TrimPrefix(color, "#")) + "," + color + ")"
}
