package web

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// Deeper chains render at this level: the column is too narrow for more.
const maxTagDepth = 3

var categoryRank = map[string]int{
	"rating": 0, "year": 1, "meta": 2, "medium": 3,
	"person": 4, "artist": 5, "copyright": 6, "character": 7,
	"species": 8, "general": 10,
}

const customCategoryRank = 9

type tagSidebarRow struct {
	models.ImageTag
	Depth      int
	Marker     string
	MarkerKind string
	MarkerHint string
	NameHint   string
	Source     string
	Orphaned   bool
	RemoveURL  string
}

// An implied row goes with its parent, so it gets a button only once orphaned.
func (r tagSidebarRow) Removable() bool { return !r.IsImplied || r.Orphaned }

type tagSidebarSection struct {
	Name         string
	Color        string
	Rows         []tagSidebarRow
	DeleteURL    string
	DeleteCount  int
	DeleteShared int
}

const (
	tagModeCategory = "category"
	tagModeSource   = "source"
)

const tagModeCookieName = "monbooru_tag_mode"

type tagSidebar struct {
	ImageID    int64
	CSRFToken  string
	Sections   []tagSidebarSection
	StaleCount int
	Mode       string
}

func normalizeTagMode(v string) string {
	if v == tagModeSource {
		return tagModeSource
	}
	return tagModeCategory
}

func readTagModeCookie(r *http.Request) string {
	c, err := r.Cookie(tagModeCookieName)
	if err != nil {
		return tagModeCategory
	}
	return normalizeTagMode(c.Value)
}

func writeTagModeCookie(w http.ResponseWriter, mode string) {
	if mode == tagModeSource {
		http.SetCookie(w, &http.Cookie{
			Name:     tagModeCookieName,
			Value:    tagModeSource,
			Path:     "/",
			HttpOnly: true,
			MaxAge:   31_536_000,
			SameSite: http.SameSiteLaxMode,
		})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: tagModeCookieName, Value: "", Path: "/", MaxAge: -1})
}

func (s *Server) buildTagSidebar(imageID int64, csrfToken, mode string, imageTags []models.ImageTag) tagSidebar {
	sb := tagSidebar{ImageID: imageID, CSRFToken: csrfToken, Mode: mode}
	if len(imageTags) == 0 {
		return sb
	}
	ledger, err := s.tagSvc().TagSourcesForImage(imageID)
	if err != nil {
		logx.Warnf("TagSourcesForImage(%d): %v", imageID, err)
	}
	taggerNames := distinctTaggerNames(imageTags, true)
	aliases := s.aliasNames(imageTags)

	rows := make(map[int64]tagSidebarRow, len(imageTags))
	for _, t := range imageTags {
		r := tagSidebarRow{
			ImageTag:  t,
			NameHint:  nameHint(t.TagName, aliases[t.TagID]),
			Source:    rowSource(t),
			RemoveURL: fmt.Sprintf("/images/%d/tags/%d", imageID, t.TagID),
		}
		annotateTagRow(&r, ledger[t.TagID], taggerNames)
		if t.Stale {
			sb.StaleCount++
		}
		rows[t.TagID] = r
	}
	implied := s.impliedUnder(imageTags)
	if mode == tagModeSource {
		sb.Sections = groupTagRowsBySource(imageID, imageTags, rows, implied, ledger, taggerNames)
	} else {
		sb.Sections = groupTagRowsByCategory(imageID, imageTags, rows, implied)
	}
	return sb
}

// A tag several sources vouch for lists under each, so its xN marker goes.
func groupTagRowsBySource(imageID int64, imageTags []models.ImageTag,
	rows map[int64]tagSidebarRow, implied map[int64][]int64,
	ledger map[int64][]tags.TagSource, taggerNames []string) []tagSidebarSection {

	byLabel := map[string][]models.ImageTag{}
	shared := map[string]int{}
	for _, t := range imageTags {
		if t.IsImplied {
			continue
		}
		labels := ledger[t.TagID]
		if len(labels) == 0 {
			// The ledger backfill's rule, which the withdrawal SQL shares.
			fallback := cmp.Or(t.TaggerName, "user")
			byLabel[fallback] = append(byLabel[fallback], t)
			continue
		}
		for _, src := range labels {
			byLabel[src.Source] = append(byLabel[src.Source], t)
			if len(labels) > 1 {
				shared[src.Source]++
			}
		}
	}

	names := slices.SortedFunc(maps.Keys(byLabel), func(a, b string) int {
		if ra, rb := rankSource(a, taggerNames), rankSource(b, taggerNames); ra != rb {
			return cmp.Compare(ra, rb)
		}
		return strings.Compare(a, b)
	})

	out := make([]tagSidebarSection, 0, len(names))
	for _, name := range names {
		group := byLabel[name]
		sortForDisplay(group)
		section := tagSidebarSection{
			Name:         name,
			DeleteCount:  len(group),
			DeleteShared: shared[name],
		}
		// Per group, so each source's copy of a parent nests its subtree.
		attached := map[int64]bool{}
		for _, t := range group {
			row := rows[t.TagID]
			if row.MarkerKind == "sources" {
				row.Marker, row.MarkerKind = "", ""
			}
			row.RemoveURL = fmt.Sprintf("/images/%d/source-contribution?source=%s&tag=%d",
				imageID, url.QueryEscape(name), t.TagID)
			section.Rows = append(section.Rows, row)
			section.Rows = append(section.Rows, nestImplied(nil, t.TagID, 1, rows, implied, attached)...)
		}
		if section.DeleteCount > 0 {
			section.DeleteURL = fmt.Sprintf("/images/%d/source-contribution?source=%s", imageID, url.QueryEscape(name))
		}
		out = append(out, section)
	}
	if orphans := orphanedImplied(imageTags, implied); len(orphans) > 0 {
		sortForDisplay(orphans)
		section := tagSidebarSection{Name: "implied"}
		for _, t := range orphans {
			row := rows[t.TagID]
			row.Orphaned = true
			section.Rows = append(section.Rows, row)
		}
		out = append(out, section)
	}
	return out
}

func rankSource(name string, taggerNames []string) int {
	switch {
	case name == "user":
		return 0
	case slices.Contains(taggerNames, name):
		return 2
	default:
		return 1
	}
}

func nameHint(name, aliases string) string {
	if aliases == "" {
		return name
	}
	return name + "\naliases: " + aliases
}

func rowSource(t models.ImageTag) string {
	switch {
	case t.IsImplied:
		return "implied"
	case t.IsAuto:
		return "auto"
	case t.TaggerName != "":
		return t.TaggerName
	default:
		return "user"
	}
}

func annotateTagRow(r *tagSidebarRow, sources []tags.TagSource, taggerNames []string) {
	r.MarkerHint = sourceHint(sources, r.TaggerName, r.Confidence)
	switch {
	case r.Stale:
		r.Marker, r.MarkerKind = "stale", "stale"
	case autoOnly(r.ImageTag, sources, taggerNames) && r.Confidence != nil:
		r.Marker, r.MarkerKind = strconv.Itoa(int(*r.Confidence*100))+"%", "conf"
	case len(sources) > 1:
		r.Marker, r.MarkerKind = "x"+strconv.Itoa(len(sources)), "sources"
	}
}

func autoOnly(t models.ImageTag, sources []tags.TagSource, taggerNames []string) bool {
	if !t.IsAuto {
		return false
	}
	for _, src := range sources {
		if !slices.Contains(taggerNames, src.Source) {
			return false
		}
	}
	return true
}

func sourceHint(sources []tags.TagSource, taggerName string, confidence *float64) string {
	lines := make([]string, 0, len(sources))
	for _, src := range sources {
		line := src.Source
		// Ledger dates are ISO, backfilled ones space-separated; keep the date.
		if i := strings.IndexAny(src.CreatedAt, "T "); i > 0 {
			line += "  " + src.CreatedAt[:i]
		}
		if confidence != nil && src.Source == taggerName {
			line += fmt.Sprintf("  %d%%", int(*confidence*100))
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func groupTagRowsByCategory(imageID int64, imageTags []models.ImageTag,
	rows map[int64]tagSidebarRow, implied map[int64][]int64) []tagSidebarSection {

	tops := make([]models.ImageTag, 0, len(imageTags))
	for _, t := range imageTags {
		if !t.IsImplied {
			tops = append(tops, t)
		}
	}
	// Sorted first: a tag two parents imply nests under the first one shown.
	sortForDisplay(tops)
	attached := map[int64]bool{}
	subtrees := make(map[int64][]tagSidebarRow, len(tops))
	for _, t := range tops {
		subtrees[t.TagID] = nestImplied(nil, t.TagID, 1, rows, implied, attached)
	}
	orphans := orphanedImplied(imageTags, implied)
	tops = append(tops, orphans...)
	sortForDisplay(tops)

	var out []tagSidebarSection
	for _, t := range tops {
		if len(out) == 0 || out[len(out)-1].Name != t.Category {
			out = append(out, tagSidebarSection{Name: t.Category, Color: t.Color})
		}
		section := &out[len(out)-1]
		row := rows[t.TagID]
		row.Orphaned = t.IsImplied
		section.Rows = append(section.Rows, row)
		section.Rows = append(section.Rows, subtrees[t.TagID]...)
		if !t.IsImplied {
			section.DeleteCount++
		}
	}
	for i := range out {
		if out[i].DeleteCount > 0 {
			out[i].DeleteURL = fmt.Sprintf("/images/%d/category-tags?cat=%s", imageID, url.QueryEscape(out[i].Name))
		}
	}
	return out
}

// Each grouping must list these, or the tag is on the image but not the page.
func orphanedImplied(imageTags []models.ImageTag, implied map[int64][]int64) []models.ImageTag {
	justified := tags.JustifiedImplied(imageTags, implied)
	var out []models.ImageTag
	for _, t := range imageTags {
		if t.IsImplied && !justified[t.TagID] {
			out = append(out, t)
		}
	}
	return out
}

func nestImplied(out []tagSidebarRow, parentID int64, depth int,
	rows map[int64]tagSidebarRow, implied map[int64][]int64, attached map[int64]bool) []tagSidebarRow {

	for _, childID := range implied[parentID] {
		if attached[childID] {
			continue
		}
		attached[childID] = true
		r := rows[childID]
		r.Depth = min(depth, maxTagDepth)
		r.NameHint += "\nimplied by " + rows[parentID].TagName
		out = append(out, r)
		out = nestImplied(out, childID, depth+1, rows, implied, attached)
	}
	return out
}

func sortForDisplay(list []models.ImageTag) {
	sort.SliceStable(list, func(i, j int) bool {
		ri, rj := rankCategory(list[i].Category), rankCategory(list[j].Category)
		if ri != rj {
			return ri < rj
		}
		if list[i].Category != list[j].Category {
			return list[i].Category < list[j].Category
		}
		return gallery.NaturalLess(strings.ToLower(list[i].TagName), strings.ToLower(list[j].TagName))
	})
}

func rankCategory(name string) int {
	if r, ok := categoryRank[name]; ok {
		return r
	}
	return customCategoryRank
}

func (s *Server) aliasNames(imageTags []models.ImageTag) map[int64]string {
	ids := make([]int64, 0, len(imageTags))
	for _, t := range imageTags {
		if !t.IsImplied {
			ids = append(ids, t.TagID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	byCanonical, err := s.tagSvc().AliasesForTagIDs(ids)
	if err != nil {
		logx.Warnf("AliasesForTagIDs: %v", err)
		return nil
	}
	out := make(map[int64]string, len(byCanonical))
	for id, list := range byCanonical {
		names := make([]string, len(list))
		for i, a := range list {
			names[i] = a.Name
		}
		sort.Strings(names)
		out[id] = strings.Join(names, ", ")
	}
	return out
}

func (s *Server) impliedUnder(imageTags []models.ImageTag) map[int64][]int64 {
	onImage := make(map[int64]bool, len(imageTags))
	ids := make([]int64, 0, len(imageTags))
	for _, t := range imageTags {
		onImage[t.TagID] = t.IsImplied
		ids = append(ids, t.TagID)
	}
	edges, err := s.tagSvc().ImplicationsForParents(ids)
	if err != nil {
		logx.Warnf("ImplicationsForParents: %v", err)
		return nil
	}
	out := make(map[int64][]int64, len(edges))
	for parent, list := range edges {
		for _, im := range list {
			if isImplied, ok := onImage[im.ImpliedID]; ok && isImplied {
				out[parent] = append(out[parent], im.ImpliedID)
			}
		}
	}
	return out
}
