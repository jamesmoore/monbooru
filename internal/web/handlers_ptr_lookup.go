package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

const ptrLookupBatch = 500

func (s *Server) ptrLookupSearchPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) || pageGalleryStale(w, r, s.activeGallery()) {
		return
	}
	s.startTagScopeRun(w, r, s.runPTRTagLookup)
}

func (s *Server) ptrLookupTagPost(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	as := strings.TrimSpace(r.FormValue("as"))
	if as != "" {
		form, ok := s.ptrSpellingForm(as)
		if !ok {
			http.Error(w, "not a tag name", http.StatusBadRequest)
			return
		}
		as = form
	}
	merge := r.FormValue("merge") != ""
	if merge && as == "" {
		http.Error(w, "not a tag name", http.StatusBadRequest)
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	if merge {
		go s.runPTRMergeSweep(id, as)
	} else {
		go s.runPTRTagSweep([]int64{id}, as)
	}
	w.WriteHeader(http.StatusAccepted)
}

// Both halves share the job slot the caller already holds.
// The spelling is exact, a bare one meaning general, as the PTR writes it.
func (s *Server) runPTRMergeSweep(id int64, spelling string) {
	catID, bare, ok := s.splitCategoryTag(spelling)
	if !ok {
		s.jobs.Fail("not a tag name: " + spelling)
		return
	}
	target, err := s.tagSvc().GetOrCreateTag(bare, catID)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}
	s.jobs.Update(0, 1, "aliasing tag…")
	if err := s.tagSvc().MergeTags(id, target.ID); err != nil {
		s.jobs.Fail(err.Error())
		return
	}
	s.active().InvalidateCaches()
	s.runPTRTagSweep([]int64{target.ID}, "")
}

func (s *Server) ptrSpellingForm(input string) (string, bool) {
	input = strings.TrimSpace(input)
	catID, bare, ok := s.splitCategoryTag(input)
	if !ok {
		return "", false
	}
	norm, err := tags.ValidateTagName(bare)
	if err != nil {
		return "", false
	}
	if catID == s.active().GeneralCategoryID {
		return norm, true
	}
	return input[:len(input)-len(bare)] + norm, true
}

type ptrLookupCand struct {
	id   int64
	name string
}

// Rating tags are immutable, so they are never swept.
func (s *Server) ptrLookupCands(ids []int64) ([]ptrLookupCand, error) {
	var cands []ptrLookupCand
	for start := 0; start < len(ids); start += ptrLookupBatch {
		placeholders, args := db.InPlaceholders(ids[start:min(start+ptrLookupBatch, len(ids))])
		batch, err := db.QueryAll(s.db().Read, func(rows *sql.Rows) (ptrLookupCand, error) {
			var c ptrLookupCand
			var cat string
			err := rows.Scan(&c.id, &c.name, &cat)
			if cat != "general" {
				c.name = cat + ":" + c.name
			}
			return c, err
		},
			`SELECT t.id, t.name, c.name FROM tags t JOIN tag_categories c ON c.id = t.category_id
			 WHERE t.id IN (`+placeholders+`) AND t.is_alias = 0 AND c.name != 'rating'`, args...)
		if err != nil {
			return nil, err
		}
		cands = append(cands, batch...)
	}
	return cands, nil
}

// An orphan spelling answers known with nothing behind it, so a connected
// candidate beats it wherever it turns up; otherwise the tag's own orphan
// name would shadow an alias holding the whole cluster.
func resolvePTRSpelling(graph map[string]ptrTagInfo, own string, aliases []string) (string, ptrTagInfo, bool) {
	bareName, bareInfo, haveBare := "", ptrTagInfo{}, false
	connected := func(name string, info ptrTagInfo) bool {
		if len(info.Aliases)+len(info.Implications)+len(info.ImpliedBy) > 0 || (info.Ideal != "" && info.Ideal != name) {
			return true
		}
		if !haveBare {
			bareName, bareInfo, haveBare = name, info, true
		}
		return false
	}
	if info, ok := graph[own]; ok && info.Known && connected(own, info) {
		return own, info, true
	}
	var first string
	for _, a := range aliases {
		info, ok := graph[a]
		if !ok || !info.Known || !connected(a, info) {
			continue
		}
		if info.Ideal == a {
			return a, info, true
		}
		if first == "" {
			first = a
		}
	}
	if first != "" {
		return first, graph[first], true
	}
	if haveBare {
		return bareName, bareInfo, true
	}
	return "", ptrTagInfo{}, false
}

func (s *Server) ptrAliasForms(ids []int64) (map[int64][]string, error) {
	aliases, err := s.tagSvc().AliasesForTagIDs(ids)
	if err != nil {
		return nil, err
	}
	forms := make(map[int64][]string, len(aliases))
	for id, rows := range aliases {
		for _, a := range rows {
			forms[id] = append(forms[id], tagFormName(a.CategoryName, a.Name))
		}
	}
	return forms, nil
}

// A spelling that answered for the tag joins its aliases, so the pull
// adopts it and the staleness sync never retires it. An `as` the operator
// named gets no alias fallback.
func (s *Server) ptrResolveChunk(ctx context.Context, chunk []ptrLookupCand, as string) (map[int64]ptrTagInfo, error) {
	if as != "" && len(chunk) == 1 {
		graph, err := s.ptrTagLookup(ctx, []string{as})
		if err != nil {
			return nil, err
		}
		info := graph[as]
		if !info.Known {
			return map[int64]ptrTagInfo{}, nil
		}
		if as != chunk[0].name {
			info.Aliases = append(info.Aliases, as)
		}
		return map[int64]ptrTagInfo{chunk[0].id: info}, nil
	}
	names := make([]string, len(chunk))
	for i, c := range chunk {
		names[i] = c.name
	}
	graph, err := s.ptrTagLookup(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]ptrTagInfo, len(chunk))
	var unknown []int64
	for _, c := range chunk {
		if info := graph[c.name]; info.Known {
			out[c.id] = info
		} else {
			unknown = append(unknown, c.id)
		}
	}
	if len(unknown) == 0 {
		return out, nil
	}
	forms, err := s.ptrAliasForms(unknown)
	if err != nil {
		return nil, err
	}
	var all []string
	for _, id := range unknown {
		all = append(all, forms[id]...)
	}
	for start := 0; start < len(all); start += ptrLookupBatch {
		part, err := s.ptrTagLookup(ctx, all[start:min(start+ptrLookupBatch, len(all))])
		if err != nil {
			return nil, err
		}
		maps.Copy(graph, part)
	}
	for _, c := range chunk {
		if _, done := out[c.id]; done {
			continue
		}
		if spelling, info, ok := resolvePTRSpelling(graph, c.name, forms[c.id]); ok {
			info.Aliases = append(info.Aliases, spelling)
			out[c.id] = info
		}
	}
	return out, nil
}

func (s *Server) runPTRTagLookup(ids []int64) {
	s.runPTRTagSweep(ids, "")
}

// Each id is swept at most once, which is what ends the follow-up rounds
// over tags the fan-in created.
func (s *Server) runPTRTagSweep(ids []int64, as string) {
	ctx := s.jobs.Context()
	lookedUpAs := as

	cands, err := s.ptrLookupCands(ids)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	// Implied-by is pulled only for the requested tags: a series is
	// implied by every character carrying it, so recursing would pull in
	// the whole graph.
	seed := make(map[int64]bool, len(cands))
	for _, c := range cands {
		seed[c.id] = true
	}

	total := len(cands)
	aliases, implications, unknown, processed, dropped, aliased, retired := 0, 0, 0, 0, 0, 0, 0
	cancelled := false
	unavailable := false
	impliedTouched := map[int64]struct{}{}
	fanOutParents := map[int64]struct{}{}
	swept := make(map[int64]struct{}, total)
	s.jobs.Update(0, total, "PTR lookup…")
	for len(cands) > 0 && !cancelled && !unavailable {
		createdTags := map[int64]struct{}{}
		for start := 0; start < len(cands) && !cancelled; start += ptrLookupBatch {
			if ctx.Err() != nil {
				cancelled = true
				break
			}
			chunk := cands[start:min(start+ptrLookupBatch, len(cands))]
			results, err := s.ptrResolveChunk(ctx, chunk, as)
			if errors.Is(err, errPTRUnavailable) {
				// The PTR going away mid-sweep is a degraded stop, not a
				// failure: what already applied stays.
				unavailable = true
				break
			}
			if err != nil {
				s.jobs.Fail("PTR lookup failed: " + err.Error())
				return
			}
			for _, c := range chunk {
				swept[c.id] = struct{}{}
				info := results[c.id]
				if !info.Known {
					// Unknown now, so every relation it pulled earlier
					// goes stale.
					unknown++
					retired += s.syncPTRStaleness(c.id, nil, nil)
				} else {
					a, i, d, al, r := s.applyPTRTagInfo(c.id, info, impliedTouched, fanOutParents, createdTags, seed[c.id])
					aliases += a
					implications += i
					dropped += d
					aliased += al
					retired += r
				}
				processed++
			}
			s.jobs.Update(processed, total, "PTR lookup…")
		}
		if cancelled || unavailable {
			break
		}
		as = ""
		var next []int64
		for id := range createdTags {
			if _, ok := swept[id]; !ok {
				next = append(next, id)
			}
		}
		if cands, err = s.ptrLookupCands(next); err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		total += len(cands)
	}

	// Inline, since the propagation job cannot start while this sweep holds
	// the runner; one pass per parent applies its full implied closure.
	for parentID := range fanOutParents {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		if err := s.fanOutImplicationsInline(ctx, parentID); err != nil {
			logx.Warnf("ptr lookup fan-out for tag %d: %v", parentID, err)
		}
	}
	if len(impliedTouched) > 0 {
		touched := make([]int64, 0, len(impliedTouched))
		for id := range impliedTouched {
			touched = append(touched, id)
		}
		if err := s.tagSvc().RecalcIDs(touched); err != nil {
			logx.Warnf("ptr lookup recalc: %v", err)
		}
	}
	s.active().InvalidateCaches()

	msg := fmt.Sprintf("PTR: added %d alias(es) and %d implication(s) across %d tag(s)", aliases, implications, processed)
	if lookedUpAs != "" {
		msg += ", looked up as " + lookedUpAs
	}
	if unknown > 0 {
		msg += fmt.Sprintf("; %d unknown to the PTR", unknown)
	}
	if dropped > 0 {
		msg += fmt.Sprintf("; %d spelling(s) not representable", dropped)
	}
	if aliased > 0 {
		msg += fmt.Sprintf("; %d relation(s) skipped, an alias here points elsewhere", aliased)
	}
	if retired > 0 {
		msg += fmt.Sprintf("; %d relation(s) no longer on the PTR", retired)
	}
	msg += "."
	if unavailable && !cancelled {
		s.jobs.Complete(fmt.Sprintf("PTR lookup stopped at %d/%d: the PTR became unavailable on monloader. %s", processed, total, msg))
		return
	}
	s.finishJob(nil, cancelled, fmt.Sprintf("PTR lookup cancelled (%d/%d processed)", processed, total), msg)
}

// The PTR ideal becomes an alias too, since locally this tag stays the
// canonical. An alias only takes an unused name, so the operator's
// catalog wins; AddImplicationFrom guards cycles and aliases.
func (s *Server) applyPTRTagInfo(tagID int64, info ptrTagInfo, impliedTouched, fanOutParents, createdTags map[int64]struct{}, pullImpliedBy bool) (aliases, implications, dropped, aliased, retired int) {
	freshAliases := map[tags.AliasKey]bool{}
	freshImplied := map[int64]bool{}
	aliasNames := info.Aliases
	if info.Ideal != "" {
		aliasNames = append([]string{info.Ideal}, aliasNames...)
	}
	for _, name := range aliasNames {
		catID, bare, ok := s.splitCategoryTag(name)
		if !ok {
			dropped++
			continue
		}
		normalized, err := tags.ValidateTagName(bare)
		if err != nil {
			dropped++
			continue
		}
		freshAliases[tags.AliasKey{CategoryID: catID, Name: normalized}] = true
		exists, err := s.tagNameExists(catID, normalized)
		if err != nil {
			logx.Warnf("ptr alias %q: %v", name, err)
			continue
		}
		if exists {
			continue
		}
		if _, err := s.tagSvc().CreateAliasFrom(normalized, catID, tagID, "ptr"); err != nil {
			logx.Warnf("ptr alias %q: %v", name, err)
			continue
		}
		aliases++
	}
	for _, name := range info.Implications {
		catID, bare, ok := s.splitCategoryTag(name)
		if !ok {
			continue
		}
		normalized, err := tags.ValidateTagName(bare)
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		exists, err := s.tagNameExists(catID, normalized)
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		implied, err := s.tagSvc().GetOrCreateTagFrom(normalized, catID, "ptr")
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		if redirected(implied, normalized, catID) {
			aliased++
			continue
		}
		freshImplied[implied.ID] = true
		if !exists {
			createdTags[implied.ID] = struct{}{}
		}
		isNew, err := s.tagSvc().AddImplicationFrom(tagID, implied.ID, "ptr")
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		if isNew {
			implications++
			impliedTouched[implied.ID] = struct{}{}
			fanOutParents[tagID] = struct{}{}
		}
	}
	if pullImpliedBy {
		for _, name := range info.ImpliedBy {
			catID, bare, ok := s.splitCategoryTag(name)
			if !ok {
				continue
			}
			normalized, err := tags.ValidateTagName(bare)
			if err != nil {
				logx.Warnf("ptr implied-by %q: %v", name, err)
				continue
			}
			// The parent is not swept: its implications reach this tag's
			// siblings (solo_futanari implies futanari) and would drag
			// the cluster in.
			parent, err := s.tagSvc().GetOrCreateTagFrom(normalized, catID, "ptr")
			if err != nil {
				logx.Warnf("ptr implied-by %q: %v", name, err)
				continue
			}
			if redirected(parent, normalized, catID) {
				aliased++
				continue
			}
			isNew, err := s.tagSvc().AddImplicationFrom(parent.ID, tagID, "ptr")
			if err != nil {
				logx.Warnf("ptr implied-by %q: %v", name, err)
				continue
			}
			if isNew {
				implications++
				impliedTouched[tagID] = struct{}{}
				fanOutParents[parent.ID] = struct{}{}
			}
		}
	}
	retired = s.syncPTRStaleness(tagID, freshAliases, freshImplied)
	return aliases, implications, dropped, aliased, retired
}

// A redirected name is an alias here: storing the edge on its canonical
// would assert one neither source declares, which the contribution dialog
// would then offer back as new.
func redirected(got *models.Tag, name string, categoryID int64) bool {
	return got.Name != name || got.CategoryID != categoryID
}

func (s *Server) syncPTRStaleness(tagID int64, freshAliases map[tags.AliasKey]bool, freshImplied map[int64]bool) int {
	retired := 0
	n, err := s.tagSvc().SyncAliasStaleness(tagID, "ptr", freshAliases)
	if err != nil {
		logx.Warnf("ptr alias staleness for tag %d: %v", tagID, err)
	}
	retired += n
	n, err = s.tagSvc().SyncImplicationStaleness(tagID, "ptr", freshImplied)
	if err != nil {
		logx.Warnf("ptr implication staleness for tag %d: %v", tagID, err)
	}
	return retired + n
}

func (s *Server) tagNameExists(catID int64, name string) (bool, error) {
	var n int
	if err := s.db().Read.QueryRow(
		`SELECT COUNT(*) FROM tags WHERE name = ? AND category_id = ?`, name, catID,
	).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Server) splitCategoryTag(input string) (catID int64, bare string, ok bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, "", false
	}
	if idx := strings.Index(input, ":"); idx > 0 {
		if name := input[idx+1:]; name != "" {
			if id, ok, err := tags.CategoryIDByName(s.db(), input[:idx]); ok && err == nil {
				return id, name, true
			}
		}
	}
	cx := s.active()
	if cx == nil || cx.GeneralCategoryID == 0 {
		return 0, "", false
	}
	return cx.GeneralCategoryID, input, true
}

func (s *Server) fanOutImplicationsInline(ctx context.Context, parentID int64) error {
	ratingCatID := s.tagSvc().RatingCategoryID()
	return s.chunkImageTagsByParent(ctx, parentID, func(tx *sql.Tx, imageID int64) error {
		return propagateAddImplication(tx, imageID, parentID, ratingCatID)
	})
}
