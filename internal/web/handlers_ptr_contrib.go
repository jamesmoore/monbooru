package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

func (s *Server) imageShaType(id int64) (sha, fileType string, ok bool) {
	err := s.db().Read.QueryRow(`SELECT sha256, file_type FROM images WHERE id = ?`, id).Scan(&sha, &fileType)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false
	}
	return sha, fileType, err == nil
}

// An empty msg is a panel's refusal: a bare 200 collapses it in place
// rather than reading as breakage.
func ptrRefuse(w http.ResponseWriter, msg string, code int) {
	if msg == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, msg, code)
}

func (s *Server) ptrTagTarget(w http.ResponseWriter, r *http.Request, open func() bool, refusal string) (int64, *models.Tag, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return 0, nil, false
	}
	if !open() {
		ptrRefuse(w, refusal, http.StatusConflict)
		return 0, nil, false
	}
	tag, ok := s.contribTag(id)
	if !ok {
		ptrRefuse(w, ptrMissText(refusal), http.StatusNotFound)
		return 0, nil, false
	}
	return id, tag, true
}

// A cbz bundle has no single file to speak about, so it refuses like a
// missing row.
func (s *Server) ptrImageTarget(w http.ResponseWriter, r *http.Request, open func() bool, refusal string) (int64, string, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return 0, "", false
	}
	if !open() {
		ptrRefuse(w, refusal, http.StatusConflict)
		return 0, "", false
	}
	sha, fileType, ok := s.imageShaType(id)
	if !ok || fileType == models.FileTypeCBZ {
		ptrRefuse(w, ptrMissText(refusal), http.StatusNotFound)
		return 0, "", false
	}
	return id, sha, true
}

func ptrMissText(refusal string) string {
	if refusal == "" {
		return ""
	}
	return "not found"
}

type contribPreviewToAdd struct {
	Tag        string `json:"tag"`
	PTR        string `json:"ptr"`
	Status     string `json:"status"`
	Note       string `json:"note"`
	UnknownTag bool   `json:"unknown_tag"`
	Color      string `json:"-"`
	// Sent is the spelling the row goes up under; Tag is the one the
	// operator sees.
	Sent string `json:"-"`
}

func (t contribPreviewToAdd) PTRDiffers() bool { return strings.ReplaceAll(t.Tag, "_", " ") != t.PTR }

type contribPreviewPTROnly struct {
	Tag          string `json:"tag"`
	PTR          string `json:"ptr"`
	Petitionable bool   `json:"petitionable"`
	Color        string `json:"-"`
}

type contribPreview struct {
	Provisional bool                    `json:"provisional"`
	ToAdd       []contribPreviewToAdd   `json:"to_add"`
	PTROnly     []contribPreviewPTROnly `json:"ptr_only"`
}

func monloaderPostJSON[T any](s *Server, ctx context.Context, path string, payload map[string]any) (*T, error) {
	body, _ := json.Marshal(payload)
	var out T
	if err := s.monloaderContribJSON(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Server) monloaderContribPreview(ctx context.Context, sha256 string, tags, implied []string) (*contribPreview, error) {
	return monloaderPostJSON[contribPreview](s, ctx, "/api/v1/ptr/contrib/preview",
		map[string]any{"sha256": sha256, "tags": tags, "implied": implied})
}

type contribSendResult struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
	Note   string `json:"note"`
}

type contribSendResponse struct {
	Results []contribSendResult `json:"results"`
	JobID   int64               `json:"job_id"`
}

func (s *Server) monloaderContribSend(ctx context.Context, origin string, items []map[string]any) (*contribSendResponse, error) {
	return monloaderPostJSON[contribSendResponse](s, ctx, "/api/v1/ptr/contrib",
		map[string]any{"commit": true, "origin": origin, "items": items})
}

func (s *Server) monloaderContribJSON(ctx context.Context, method, path string, body []byte, out any) error {
	resp, err := s.monloader().Do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return errPTRUnavailable
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("monloader returned %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func tagFormName(cat, name string) string {
	if cat != "general" {
		return cat + ":" + name
	}
	return name
}

func (s *Server) contribTagColor() func(string) string {
	colors := map[string]string{}
	if cats, err := s.tagSvc().ListCategories(); err == nil {
		for _, c := range cats {
			colors[c.Name] = c.Color
		}
	}
	return func(tag string) string {
		if i := strings.Index(tag, ":"); i > 0 {
			if color, ok := colors[tag[:i]]; ok {
				return color
			}
		}
		return colors["general"]
	}
}

// Context tags are never offered as adds but keep what the image already
// shows out of the petition candidates. monloader refuses the rating
// namespace, and a peer derives the derived-only tags from its own file.
func (s *Server) contribStorageTags(imageTags []models.ImageTag, sources map[int64][]tags.TagSource) (storage, implied []string) {
	for _, t := range imageTags {
		if t.IsImplied || t.Category == "rating" || derivedOnly(t, sources[t.TagID]) {
			implied = append(implied, tagFormName(t.Category, t.TagName))
			continue
		}
		storage = append(storage, tagFormName(t.Category, t.TagName))
	}
	return storage, implied
}

func derivedOnly(t models.ImageTag, ledger []tags.TagSource) bool {
	if len(ledger) == 0 {
		return t.TaggerName == models.TagSourceMonbooru
	}
	for _, src := range ledger {
		if src.Source != models.TagSourceMonbooru {
			return false
		}
	}
	return true
}

func (s *Server) imageContribPreview(rctx context.Context, id int64, sha string) (*contribPreview, error) {
	_, imageTags, _ := s.tagSvc().GetImageTags(id)
	ledger, err := s.tagSvc().TagSourcesForImage(id)
	if err != nil {
		logx.Warnf("TagSourcesForImage: %v", err)
	}
	storage, implied := s.contribStorageTags(imageTags, ledger)
	byTag, byAlias := s.imageTagAliases(imageTags)
	ctx, cancel := context.WithTimeout(rctx, 8*time.Second)
	defer cancel()
	sent, local := s.ptrSubmitSpellings(ctx, storage, byTag)
	preview, err := s.monloaderContribPreview(ctx, sha, sent, implied)
	if err != nil {
		return nil, err
	}
	for i, t := range preview.ToAdd {
		preview.ToAdd[i].Sent = t.Tag
		if own, ok := local[t.Tag]; ok {
			preview.ToAdd[i].Tag = own
		}
	}
	foldAliasedPTRTags(preview, byAlias)
	return preview, nil
}

// The graph endpoint refuses while the index syncs, when the panel still
// renders: an error means no substitution, not a failure.
func (s *Server) ptrSubmitSpellings(ctx context.Context, storage []string, byTag map[string][]string) ([]string, map[string]string) {
	var names []string
	for _, form := range storage {
		if len(byTag[form]) > 0 {
			names = append(names, form)
			names = append(names, byTag[form]...)
		}
	}
	if len(names) == 0 {
		return storage, nil
	}
	graph := map[string]ptrTagInfo{}
	for start := 0; start < len(names); start += ptrLookupBatch {
		part, err := s.ptrTagLookup(ctx, names[start:min(start+ptrLookupBatch, len(names))])
		if err != nil {
			return storage, nil
		}
		maps.Copy(graph, part)
	}
	sent := make([]string, len(storage))
	taken := make(map[string]bool, len(storage))
	for i, form := range storage {
		sent[i], taken[form] = form, true
	}
	local := map[string]string{}
	for i, form := range storage {
		if len(byTag[form]) == 0 {
			continue
		}
		// A spelling another tag on the image already occupies would go
		// up twice and pair the wrong row with it.
		if spelling, _, ok := resolvePTRSpelling(graph, form, byTag[form]); ok && !taken[spelling] {
			sent[i], taken[spelling], local[spelling] = spelling, true, form
		}
	}
	return sent, local
}

// Without this, a PTR spelling pulled here as an alias shows as a petition
// and the operator's own spelling as a new upload of the same tag.
func foldAliasedPTRTags(preview *contribPreview, byAlias map[string]string) {
	if len(byAlias) == 0 {
		return
	}
	carried := map[string]bool{}
	kept := preview.PTROnly[:0]
	for _, p := range preview.PTROnly {
		if local, ok := byAlias[p.Tag]; ok {
			carried[local] = true
			continue
		}
		kept = append(kept, p)
	}
	preview.PTROnly = kept
	for i, t := range preview.ToAdd {
		if t.Status == "new" && carried[t.Tag] {
			preview.ToAdd[i].Status = "known"
		}
	}
}

// A name a pre-widening catalog folded is keyed under both shapes.
func (s *Server) imageTagAliases(imageTags []models.ImageTag) (byTag map[string][]string, byAlias map[string]string) {
	ids := make([]int64, 0, len(imageTags))
	for _, t := range imageTags {
		ids = append(ids, t.TagID)
	}
	byCanonical, err := s.tagSvc().AliasesForTagIDs(ids)
	if err != nil {
		logx.Warnf("AliasesForTagIDs: %v", err)
		return nil, nil
	}
	byTag, out := map[string][]string{}, map[string]string{}
	for _, list := range byCanonical {
		for _, a := range list {
			form := tagFormName(a.CategoryName, a.Name)
			canonical := tagFormName(a.CanonicalCategoryName, a.CanonicalName)
			byTag[canonical] = append(byTag[canonical], form)
			out[form] = canonical
			out[tags.LegacyFold(form)] = canonical
		}
	}
	return byTag, out
}

// A tag the PTR holds that the ledger does not credit to it is still work
// for a pull, which records the attribution.
func (s *Server) ptrUnattributed(id int64, preview *contribPreview) []string {
	known := map[string]bool{}
	for _, t := range preview.ToAdd {
		if t.Status == "known" {
			known[t.Tag] = true
		}
	}
	if len(known) == 0 {
		return nil
	}
	ledger, err := s.tagSvc().TagSourcesForImage(id)
	if err != nil {
		return nil
	}
	_, imageTags, _ := s.tagSvc().GetImageTags(id)
	var out []string
	for _, t := range imageTags {
		name := tagFormName(t.Category, t.TagName)
		if !known[name] || slices.ContainsFunc(ledger[t.TagID], isPTRSource) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func isPTRSource(src tags.TagSource) bool { return strings.EqualFold(src.Source, "ptr") }

func (s *Server) ptrContribPanel(w http.ResponseWriter, r *http.Request) {
	id, sha, ok := s.ptrImageTarget(w, r, s.contribReadOpen, "")
	if !ok {
		return
	}
	preview, err := s.imageContribPreview(r.Context(), id, sha)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	var newTags, petitionTags, unknownTags []string
	hasKnown := false
	for _, t := range preview.ToAdd {
		switch t.Status {
		case "new":
			newTags = append(newTags, t.Tag)
			if t.UnknownTag {
				unknownTags = append(unknownTags, t.Tag)
			}
		case "known":
			hasKnown = true
		}
	}
	for _, p := range preview.PTROnly {
		if p.Petitionable {
			petitionTags = append(petitionTags, p.Tag)
		}
	}
	canContribute, contribHint := s.contribHint()
	unattributed := s.ptrUnattributed(id, preview)
	s.renderTemplate(w, "partials/ptr_contrib_panel.html", map[string]any{
		"ImageID":           id,
		"NewCount":          len(newTags),
		"PetitionCount":     len(petitionTags),
		"AllKnown":          hasKnown,
		"NewTip":            strings.Join(newTags, "\n"),
		"PetitionTip":       strings.Join(petitionTags, "\n"),
		"UnknownCount":      len(unknownTags),
		"UnknownTip":        strings.Join(unknownTags, "\n"),
		"UnattributedCount": len(unattributed),
		"UnattributedTip":   strings.Join(unattributed, "\n"),
		"CanContribute":     canContribute,
		"CanPull":           s.ptrPullOpen() && (len(petitionTags) > 0 || len(unattributed) > 0),
		"ContribHint":       contribHint,
		"Provisional":       preview.Provisional,
		"FailedUploads":     s.mlStatus.Seed().ContribFailed,
		"Monloader":         s.monloaderWebBase(),
		"CSRFToken":         s.csrfToken(sessionFromContext(r.Context())),
	})
}

func (s *Server) ptrContribDialog(w http.ResponseWriter, r *http.Request) {
	id, sha, ok := s.ptrImageTarget(w, r, s.contribGateOpen, "contributions unavailable")
	if !ok {
		return
	}
	preview, err := s.imageContribPreview(r.Context(), id, sha)
	if err != nil {
		http.Error(w, "contributions unavailable", http.StatusConflict)
		return
	}
	color := s.contribTagColor()
	var actionable, known, queued, ineligible []contribPreviewToAdd
	for _, t := range preview.ToAdd {
		t.Color = color(t.Tag)
		switch t.Status {
		case "new":
			actionable = append(actionable, t)
		case "known":
			known = append(known, t)
		case "unsent":
			queued = append(queued, t)
		default:
			ineligible = append(ineligible, t)
		}
	}
	var petitionable, petitioned []contribPreviewPTROnly
	for _, p := range preview.PTROnly {
		p.Color = color(p.Tag)
		if p.Petitionable {
			petitionable = append(petitionable, p)
		} else {
			petitioned = append(petitioned, p)
		}
	}
	s.renderTemplate(w, "partials/ptr_contrib_dialog.html", map[string]any{
		"ImageID":      id,
		"SHA256":       sha,
		"Actionable":   actionable,
		"Known":        known,
		"Queued":       queued,
		"Ineligible":   ineligible,
		"Petitionable": petitionable,
		"Petitioned":   petitioned,
		"Provisional":  preview.Provisional,
		"CSRFToken":    s.csrfToken(sessionFromContext(r.Context())),
	})
}

func (s *Server) ptrContribSend(w http.ResponseWriter, r *http.Request) {
	id, sha, ok := s.ptrImageTarget(w, r, s.contribGateOpen, "contributions unavailable")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// After a gallery switch the id can resolve to another file; the
	// ticked tags belong to the one previewed.
	if want := r.FormValue("sha256"); want != "" && want != sha {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "this image changed; reopen the dialog"})
		return
	}
	petitionReason := strings.TrimSpace(r.FormValue("petition_reason"))
	if len(r.Form["petition"]) > 0 && petitionReason == "" {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "a reason is required to petition a removal"})
		return
	}
	var items []map[string]any
	for _, tag := range r.Form["add"] {
		items = append(items, map[string]any{"kind": "mapping_add", "sha256": sha, "tag": tag})
	}
	for _, tag := range r.Form["petition"] {
		items = append(items, map[string]any{
			"kind": "mapping_petition", "sha256": sha, "tag": tag, "reason": petitionReason,
		})
	}
	s.sendContribItems(w, r, "image "+strconv.FormatInt(id, 10), items)
}

func (s *Server) sendContribItems(w http.ResponseWriter, r *http.Request, scope string, items []map[string]any) {
	if len(items) == 0 {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "nothing selected"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := s.monloaderContribSend(ctx, scope, items)
	if err != nil {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "contributions unavailable"})
		return
	}
	s.renderContribReceipt(w, items, resp)
}

func (s *Server) renderContribReceipt(w http.ResponseWriter, items []map[string]any, resp *contribSendResponse) {
	// Verdicts pair with items by position, so a count mismatch would
	// mislabel them.
	if len(resp.Results) != len(items) {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{
			"Err": fmt.Sprintf("monloader answered %d of %d items; check its contribution history", len(resp.Results), len(items)),
		})
		return
	}
	sent, refused := 0, 0
	for _, res := range resp.Results {
		if res.Result == "staged" {
			sent++
		} else {
			refused++
		}
	}
	adds, petitions, refusedRows := contribReceipt(items, resp.Results)
	w.Header().Set("HX-Retarget", "#ptr-contrib-form")
	w.Header().Set("HX-Reswap", "innerHTML")
	s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{
		"Sent":            sent,
		"Refused":         refused,
		"StagedAdds":      adds,
		"StagedPetitions": petitions,
		"RefusedRows":     refusedRows,
		"JobID":           resp.JobID,
		"Monloader":       s.monloaderWebBase(),
	})
}

type contribReceiptRow struct {
	Label string
	Note  string
}

func contribReceipt(items []map[string]any, results []contribSendResult) (adds, petitions, refused []contribReceiptRow) {
	for i := 0; i < min(len(items), len(results)); i++ {
		kind, _ := items[i]["kind"].(string)
		var label string
		switch {
		case strings.HasPrefix(kind, "mapping"):
			label, _ = items[i]["tag"].(string)
		case strings.HasPrefix(kind, "sibling"):
			label = fmt.Sprintf("%v -> %v", items[i]["bad"], items[i]["good"])
		default:
			label = fmt.Sprintf("%v => %v", items[i]["child"], items[i]["parent"])
		}
		row := contribReceiptRow{Label: label}
		switch {
		case results[i].Result != "staged":
			row.Note = results[i].Result
			if results[i].Note != "" {
				row.Note += " - " + results[i].Note
			}
			refused = append(refused, row)
		case strings.Contains(kind, "petition"):
			petitions = append(petitions, row)
		default:
			adds = append(adds, row)
		}
	}
	return adds, petitions, refused
}

func (s *Server) contribGateOpen() bool {
	if !s.pairedWith("monloader") {
		return false
	}
	ml := s.mlStatus.Seed()
	return ml.PTR && ml.Contrib
}

// A syncing index still answers diffs, marked provisional.
func (s *Server) contribReadOpen() bool {
	if !s.pairedWith("monloader") {
		return false
	}
	ml := s.mlStatus.Seed()
	return ml.PTR || ml.PTRSyncing
}

// A pull rides the lookup path, which monloader refuses until the index
// is caught up.
func (s *Server) ptrPullOpen() bool {
	return s.mlStatus.Seed().PTR
}

func (s *Server) contribHint() (bool, string) {
	ml := s.mlStatus.Seed()
	switch {
	case ml.Contrib:
		return true, ""
	case ml.PTRSyncing:
		return false, "the Public Tag Repository is still syncing"
	case ml.ContribBanned:
		return false, "the contribution account is banned"
	default:
		return false, "a contribution account in monloader is required to contribute"
	}
}

type pairPreview struct {
	APtr        string `json:"a_ptr"`
	BPtr        string `json:"b_ptr"`
	Direction   string `json:"direction"`
	Note        string `json:"note"`
	Provisional bool   `json:"provisional"`
}

func (s *Server) monloaderPairPreview(ctx context.Context, kind, a, b string) (*pairPreview, error) {
	return monloaderPostJSON[pairPreview](s, ctx, "/api/v1/ptr/contrib/pair-preview",
		map[string]any{"kind": kind, "a": a, "b": b})
}

// A and B follow hydrus: sibling A=bad (alias), B=good; parent A=child
// (the carrying tag), B=parent (the implied tag).
type tagPairRow struct {
	Kind           string
	A, B           string
	AColor, BColor string
	Rel            string
	Direction      string
	Note           string
}

// Space delimits the fields because the tag charset excludes whitespace,
// unlike '|'.
func (p tagPairRow) Value() string { return p.Kind + " " + p.A + " " + p.B }

func (p tagPairRow) Label() string {
	if p.Kind == "sibling" {
		return p.A + " -> " + p.B
	}
	return p.A + " => " + p.B
}

type tagContribDiff struct {
	Local, PTROnly []tagPairRow
	Pullable       bool
	Provisional    bool
	KnownAs        string
	Unknown        bool
	Empty          bool
	IdealElsewhere string
}

func (s *Server) tagContribRows(ctx context.Context, id int64, tag *models.Tag) (*tagContribDiff, error) {
	tagForm := tagFormName(tag.CategoryName, tag.Name)
	aliases, err := s.tagSvc().AliasesForTagIDs([]int64{id})
	if err != nil {
		return nil, err
	}
	implications, err := s.tagSvc().ListImplications(id)
	if err != nil {
		return nil, err
	}
	impliedBy, err := s.tagSvc().ImpliedBy(id)
	if err != nil {
		return nil, err
	}
	d := &tagContribDiff{}
	localAlias, localImplied := map[string]bool{}, map[string]bool{}
	var aliasForms []string
	for _, a := range aliases[id] {
		form := tagFormName(a.CategoryName, a.Name)
		d.Local = append(d.Local, tagPairRow{Kind: "sibling", A: form, B: tagForm, Rel: "alias of this"})
		localAlias[form] = true
		aliasForms = append(aliasForms, form)
	}
	for _, im := range impliedBy {
		d.Local = append(d.Local, tagPairRow{Kind: "parent", A: tagFormName(im.ParentCategoryName, im.ParentName), B: tagForm, Rel: "implied by"})
	}
	for _, im := range implications {
		d.Local = append(d.Local, tagPairRow{Kind: "parent", A: tagForm, B: tagFormName(im.ImpliedCategoryName, im.ImpliedName), Rel: "implies"})
		localImplied[tagFormName(im.ImpliedCategoryName, im.ImpliedName)] = true
	}
	names := append([]string{tagForm}, aliasForms...)
	if len(names) > ptrLookupBatch {
		names = names[:ptrLookupBatch]
	}
	graph, err := s.ptrTagLookup(ctx, names)
	if err != nil {
		return nil, err
	}
	known, info, ok := resolvePTRSpelling(graph, tagForm, aliasForms)
	if ok && known != tagForm {
		d.KnownAs = known
	}
	var toPreview []*tagPairRow
	for i := range d.Local {
		// Known only under another spelling, pair-preview reads every
		// local relation as new; those the cluster already carries would
		// propose flipping the ideal, so they fold as covered.
		if d.KnownAs != "" && s.heldByCluster(d.Local[i], tagForm, known, info) {
			d.Local[i].Direction = "covered"
			continue
		}
		toPreview = append(toPreview, &d.Local[i])
	}
	previews, err := s.pairPreviews(ctx, toPreview)
	if err != nil {
		return nil, err
	}
	for i, row := range toPreview {
		row.Direction, row.Note = previews[i].Direction, previews[i].Note
		d.Provisional = d.Provisional || previews[i].Provisional
	}
	if !ok {
		d.Unknown = true
		return d, nil
	}
	d.Pullable = s.pullWouldAdd(id, info, known)
	d.Empty = len(info.Aliases) == 0 && len(info.Implications) == 0 && len(info.ImpliedBy) == 0 &&
		(info.Ideal == "" || info.Ideal == known)
	// Only pairs the index holds (petition or pending) qualify; the rest
	// is sibling-chain noise. Rows are keyed on the spelling the PTR
	// answered for, the name the index holds them under.
	var candidates []tagPairRow
	for _, a := range info.Aliases {
		if a == "" || a == known || localAlias[s.localForm(a)] {
			continue
		}
		candidates = append(candidates, tagPairRow{Kind: "sibling", A: a, B: known, Rel: "alias of this"})
	}
	// The ideal is never a petition row: a pull adopts it as an alias
	// here, or leaves it alone when another tag here holds the name.
	if info.Ideal != "" && info.Ideal != known && !localAlias[s.localForm(info.Ideal)] && s.tagFormExists(info.Ideal) {
		d.IdealElsewhere = info.Ideal
	}
	for _, im := range info.Implications {
		if im == "" || im == known || localImplied[s.localForm(im)] {
			continue
		}
		// The pull skips an endpoint that is an alias here, so the edge
		// has no local form to disagree with.
		if isAlias, _, _, _, ok := s.tagRowByForm(im); ok && isAlias {
			continue
		}
		candidates = append(candidates, tagPairRow{Kind: "parent", A: known, B: im, Rel: "implies"})
	}
	toPreview = toPreview[:0]
	for i := range candidates {
		// A petition needs a local judgement, which an endpoint the
		// catalog has never seen cannot give.
		other := candidates[i].A
		if other == known {
			other = candidates[i].B
		}
		if s.tagFormExists(other) {
			toPreview = append(toPreview, &candidates[i])
		}
	}
	previews, err = s.pairPreviews(ctx, toPreview)
	if err != nil {
		return nil, err
	}
	for i, row := range toPreview {
		if previews[i].Direction != "petition" && previews[i].Direction != "pending" {
			continue
		}
		row.Direction, row.Note = previews[i].Direction, previews[i].Note
		d.PTROnly = append(d.PTROnly, *row)
	}
	return d, nil
}

// Each preview is a monloader round trip and a hub tag has dozens, so a
// few run at once; the first failure stops the rest.
func (s *Server) pairPreviews(ctx context.Context, rows []*tagPairRow) ([]*pairPreview, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([]*pairPreview, len(rows))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var failed sync.Once
	var firstErr error
	for i, row := range rows {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			preview, err := s.monloaderPairPreview(ctx, row.Kind, row.A, row.B)
			if err != nil {
				failed.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			out[i] = preview
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, ctx.Err()
}

func (s *Server) localForm(name string) string {
	if form, ok := s.ptrSpellingForm(name); ok {
		return form
	}
	return name
}

func (s *Server) heldByCluster(row tagPairRow, tagForm, known string, info ptrTagInfo) bool {
	names := func(list []string) bool {
		return slices.ContainsFunc(list, func(n string) bool { return s.localForm(n) == row.A })
	}
	switch {
	case row.Kind == "sibling":
		return row.A == known || row.A == s.localForm(info.Ideal) || names(info.Aliases)
	case row.A == tagForm:
		return slices.ContainsFunc(info.Implications, func(n string) bool { return s.localForm(n) == row.B })
	default:
		return names(info.ImpliedBy)
	}
}

// Uses applyPTRTagInfo's normalization and row lookups: comparing
// spellings as strings would keep offering a pull that adopts nothing.
func (s *Server) pullWouldAdd(tagID int64, info ptrTagInfo, tagForm string) bool {
	aliasFree := func(name string) bool {
		catID, bare, ok := s.splitCategoryTag(name)
		if !ok {
			return false
		}
		norm, err := tags.ValidateTagName(bare)
		if err != nil {
			return false
		}
		exists, err := s.tagNameExists(catID, norm)
		return err == nil && !exists
	}
	for _, a := range info.Aliases {
		if a != "" && a != tagForm && aliasFree(a) {
			return true
		}
	}
	if info.Ideal != "" && info.Ideal != tagForm && aliasFree(info.Ideal) {
		return true
	}
	for _, im := range info.Implications {
		if im != "" && im != tagForm && s.edgeWouldAdd(tagID, im, true) {
			return true
		}
	}
	for _, im := range info.ImpliedBy {
		if im != "" && im != tagForm && s.edgeWouldAdd(tagID, im, false) {
			return true
		}
	}
	return false
}

func (s *Server) edgeWouldAdd(tagID int64, name string, implied bool) bool {
	catID, bare, ok := s.splitCategoryTag(name)
	if !ok {
		return false
	}
	norm, err := tags.ValidateTagName(bare)
	if err != nil {
		return false
	}
	var id int64
	var isAlias int
	err = s.db().Read.QueryRow(
		`SELECT id, is_alias FROM tags WHERE name = ? AND category_id = ?`, norm, catID,
	).Scan(&id, &isAlias)
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	if err != nil || isAlias == 1 {
		return false
	}
	parent, child := tagID, id
	if !implied {
		parent, child = id, tagID
	}
	var n int
	err = s.db().Read.QueryRow(
		`SELECT COUNT(*) FROM tag_implications WHERE parent_tag_id = ? AND implied_tag_id = ?`, parent, child,
	).Scan(&n)
	return err == nil && n == 0
}

func (s *Server) tagFormExists(form string) bool {
	catID, bare, ok := s.splitCategoryTag(form)
	if !ok {
		return false
	}
	exists, err := s.tagNameExists(catID, bare)
	return err == nil && exists
}

func (s *Server) contribTag(id int64) (*models.Tag, bool) {
	tag, err := s.tagSvc().GetTag(id)
	if err != nil || tag.IsAlias || tag.CategoryName == "rating" {
		return nil, false
	}
	return tag, true
}

func (s *Server) tagContribPreview(rctx context.Context, id int64, tag *models.Tag) (*tagContribDiff, error) {
	ctx, cancel := context.WithTimeout(rctx, 8*time.Second)
	defer cancel()
	return s.tagContribRows(ctx, id, tag)
}

func (s *Server) tagPtrContribPanel(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.contribReadOpen, "")
	if !ok {
		return
	}
	diff, err := s.tagContribPreview(r.Context(), id, tag)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	var newTags, petitionTags []string
	for _, p := range diff.Local {
		if p.Direction == "suggest" {
			newTags = append(newTags, p.Label())
		}
	}
	for _, p := range diff.PTROnly {
		if p.Direction == "petition" {
			petitionTags = append(petitionTags, p.Label())
		}
	}
	canContribute, contribHint := s.contribHint()
	s.renderTemplate(w, "partials/tag_ptr_contrib_panel.html", map[string]any{
		"TagID":          id,
		"NewCount":       len(newTags),
		"PetitionCount":  len(petitionTags),
		"Pullable":       diff.Pullable && s.ptrPullOpen(),
		"NewTip":         strings.Join(newTags, "\n"),
		"PetitionTip":    strings.Join(petitionTags, "\n"),
		"KnownAs":        diff.KnownAs,
		"Unknown":        diff.Unknown,
		"Empty":          diff.Empty,
		"IdealElsewhere": diff.IdealElsewhere,
		"CanContribute":  canContribute,
		"ContribHint":    contribHint,
		"Provisional":    diff.Provisional,
		"FailedUploads":  s.mlStatus.Seed().ContribFailed,
		"Monloader":      s.monloaderWebBase(),
	})
}

func (s *Server) tagPtrContribDialog(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.contribGateOpen, "contributions unavailable")
	if !ok {
		return
	}
	diff, err := s.tagContribPreview(r.Context(), id, tag)
	if err != nil {
		http.Error(w, "contributions unavailable", http.StatusConflict)
		return
	}
	color := s.contribTagColor()
	paint := func(p tagPairRow) tagPairRow {
		p.AColor, p.BColor = color(p.A), color(p.B)
		return p
	}
	var actionable, pending, inSync, ineligible []tagPairRow
	for _, p := range diff.Local {
		p = paint(p)
		switch p.Direction {
		case "suggest":
			actionable = append(actionable, p)
		case "pending":
			pending = append(pending, p)
		case "petition", "covered":
			// Held on the PTR too, directly or through the child's parent
			// closure.
			inSync = append(inSync, p)
		default:
			ineligible = append(ineligible, p)
		}
	}
	var petitionable, petitioned []tagPairRow
	for _, p := range diff.PTROnly {
		p = paint(p)
		if p.Direction == "petition" {
			petitionable = append(petitionable, p)
		} else {
			petitioned = append(petitioned, p)
		}
	}
	s.renderTemplate(w, "partials/tag_ptr_contrib_dialog.html", map[string]any{
		"TagID":        id,
		"TagName":      tag.Name,
		"Actionable":   actionable,
		"Pending":      pending,
		"InSync":       inSync,
		"Ineligible":   ineligible,
		"Petitionable": petitionable,
		"Petitioned":   petitioned,
		"Provisional":  diff.Provisional,
		"CSRFToken":    s.csrfToken(sessionFromContext(r.Context())),
	})
}

func (s *Server) tagPtrContribSend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !s.contribGateOpen() {
		http.Error(w, "contributions unavailable", http.StatusConflict)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var items []map[string]any
	appendPairs := func(values []string, direction, reason string) {
		for _, v := range values {
			parts := strings.SplitN(v, " ", 3)
			if len(parts) != 3 {
				continue
			}
			if item := pairSendItem(parts[0], direction, parts[1], parts[2], reason); item != nil {
				items = append(items, item)
			}
		}
	}
	suggestReason := strings.TrimSpace(r.FormValue("suggest_reason"))
	petitionReason := strings.TrimSpace(r.FormValue("petition_reason"))
	if (len(r.Form["pair"]) > 0 && suggestReason == "") || (len(r.Form["pair_petition"]) > 0 && petitionReason == "") {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "a reason is required"})
		return
	}
	appendPairs(r.Form["pair"], "suggest", suggestReason)
	appendPairs(r.Form["pair_petition"], "petition", petitionReason)
	s.sendContribItems(w, r, "tag "+strconv.FormatInt(id, 10), items)
}

func pairSendItem(kind, direction, a, b, reason string) map[string]any {
	suggest, petition := "sibling", "sibling_petition"
	aKey, bKey := "bad", "good"
	if kind == "parent" {
		suggest, petition = "parent", "parent_petition"
		aKey, bKey = "child", "parent"
	}
	var itemKind string
	switch direction {
	case "suggest":
		itemKind = suggest
	case "petition":
		itemKind = petition
	default:
		return nil
	}
	return map[string]any{"kind": itemKind, aKey: a, bKey: b, "reason": reason}
}

type ptrLookupRow struct {
	Name, Color string
	Class       string
	Note        string
}

type ptrLookupGroup struct {
	Title, Note string
	Rows        []ptrLookupRow
	Adds        int
}

func (s *Server) ptrNameValid(name string) bool {
	_, bare, ok := s.splitCategoryTag(name)
	if !ok {
		return false
	}
	_, err := tags.ValidateTagName(bare)
	return err == nil
}

func (s *Server) tagRowByForm(form string) (isAlias bool, canonicalID int64, canonicalName string, usage int, ok bool) {
	catID, bare, _ := s.splitCategoryTag(form)
	norm, _ := tags.ValidateTagName(bare)
	var alias int
	var canon sql.NullInt64
	var canonName sql.NullString
	err := s.db().Read.QueryRow(
		`SELECT t.is_alias, t.canonical_tag_id, c.name, t.usage_count
		 FROM tags t LEFT JOIN tags c ON c.id = t.canonical_tag_id
		 WHERE t.name = ? AND t.category_id = ?`, norm, catID,
	).Scan(&alias, &canon, &canonName, &usage)
	if err != nil {
		return false, 0, "", 0, false
	}
	return alias == 1, canon.Int64, canonName.String, usage, true
}

func (s *Server) ptrLookupAliasRow(tagID int64, name string, color func(string) string) ptrLookupRow {
	row := ptrLookupRow{Name: name, Color: color(name)}
	if !s.ptrNameValid(name) {
		row.Class, row.Note = "ptr-row-refused", "not representable"
		return row
	}
	isAlias, canonicalID, canonicalName, usage, exists := s.tagRowByForm(name)
	switch {
	case !exists:
		row.Class, row.Note = "ptr-row-staged", "new alias"
	case isAlias && canonicalID == tagID:
		row.Class, row.Note = "ptr-row-ctx", "already an alias here"
	case isAlias:
		row.Class, row.Note = "ptr-row-refused", "an alias here of "+canonicalName
	default:
		row.Class, row.Note = "ptr-row-refused", fmt.Sprintf("a tag here (usage %d)", usage)
	}
	return row
}

func (s *Server) ptrLookupImplRow(name string, declared map[string]bool, color func(string) string) ptrLookupRow {
	row := ptrLookupRow{Name: name, Color: color(name)}
	if declared[s.localForm(name)] {
		row.Class, row.Note = "ptr-row-ctx", "declared"
		return row
	}
	if !s.ptrNameValid(name) {
		row.Class, row.Note = "ptr-row-refused", "not representable"
		return row
	}
	isAlias, _, canonicalName, _, exists := s.tagRowByForm(name)
	switch {
	case !exists:
		row.Class, row.Note = "ptr-row-staged", "new implication, new tag"
	case isAlias:
		row.Class, row.Note = "ptr-row-refused", "an alias here of "+canonicalName
	default:
		row.Class, row.Note = "ptr-row-staged", "new implication"
	}
	return row
}

func (s *Server) ptrLookupGroups(id int64, tagForm, spelling string, info ptrTagInfo) ([]ptrLookupGroup, error) {
	implications, err := s.tagSvc().ListImplications(id)
	if err != nil {
		return nil, err
	}
	impliedBy, err := s.tagSvc().ImpliedBy(id)
	if err != nil {
		return nil, err
	}
	localImplied, localImpliedBy := map[string]bool{}, map[string]bool{}
	for _, im := range implications {
		localImplied[tagFormName(im.ImpliedCategoryName, im.ImpliedName)] = true
	}
	for _, im := range impliedBy {
		localImpliedBy[tagFormName(im.ParentCategoryName, im.ParentName)] = true
	}
	color := s.contribTagColor()
	seen := map[string]bool{tagForm: true}
	aliases := ptrLookupGroup{Title: "aliases", Note: "would become aliases of " + tagForm}
	for _, name := range append([]string{spelling, info.Ideal}, info.Aliases...) {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		row := s.ptrLookupAliasRow(id, name, color)
		if row.Class == "ptr-row-staged" {
			aliases.Adds++
		}
		aliases.Rows = append(aliases.Rows, row)
	}
	implies := s.ptrLookupImplGroup(ptrLookupGroup{Title: "implies"},
		info.Implications, localImplied, color, tagForm, spelling)
	implied := s.ptrLookupImplGroup(ptrLookupGroup{Title: "implied by", Note: "direct children only, at most 200"},
		info.ImpliedBy, localImpliedBy, color, tagForm, spelling)
	var groups []ptrLookupGroup
	for _, g := range []ptrLookupGroup{aliases, implies, implied} {
		if len(g.Rows) > 0 {
			groups = append(groups, g)
		}
	}
	return groups, nil
}

func (s *Server) ptrLookupImplGroup(g ptrLookupGroup, names []string, declared map[string]bool, color func(string) string, tagForm, spelling string) ptrLookupGroup {
	for _, name := range names {
		if name == "" || name == tagForm || name == spelling {
			continue
		}
		row := s.ptrLookupImplRow(name, declared, color)
		if row.Class == "ptr-row-staged" {
			g.Adds++
		}
		g.Rows = append(g.Rows, row)
	}
	return g
}

func (s *Server) ptrLookupLocalStatus(id int64, tagForm, spelling string) (status string, aliasable bool) {
	if spelling == tagForm {
		return "this tag", false
	}
	isAlias, canonicalID, canonicalName, usage, exists := s.tagRowByForm(spelling)
	switch {
	case !exists:
		return "not in this catalog", true
	case isAlias && canonicalID == id:
		return "alias of this tag", false
	case isAlias:
		return "alias of " + canonicalName, true
	default:
		return fmt.Sprintf("a tag here (usage %d)", usage), true
	}
}

const ptrSearchLimit = 20

// Longest first: an operator's spelling is usually the repository's with
// something added.
func ptrSpellingStems(form string) []string {
	cat, name, qualified := strings.Cut(form, ":")
	if !qualified {
		cat, name = "", form
	}
	var out []string
	seen := map[string]bool{}
	push := func(n string) {
		if len(n) < 4 || seen[n] {
			return
		}
		seen[n] = true
		if cat != "" {
			n = cat + ":" + n
		}
		out = append(out, n)
	}
	push(name)
	stem := name
	for strings.HasSuffix(stem, ")") && strings.Contains(stem, "_(") {
		stem = stem[:strings.LastIndex(stem, "_(")]
		push(stem)
	}
	for parts := strings.Split(stem, "_"); len(parts) > 1; {
		parts = parts[:len(parts)-1]
		push(strings.Join(parts, "_"))
	}
	return out
}

// The tag's own name often sits in the index as an orphan, so a stem
// whose clusters carry relations beats the first stem that answers.
func (s *Server) ptrSeedSearch(ctx context.Context, tagForm string) (clusters []ptrCluster, stem string, truncated bool, err error) {
	for _, candidate := range ptrSpellingStems(tagForm) {
		got, cut, err := s.ptrSpellingSearch(ctx, candidate, "", ptrSearchLimit)
		if err != nil {
			return nil, "", false, err
		}
		if len(got) == 0 {
			continue
		}
		if clusters == nil {
			clusters, stem, truncated = got, candidate, cut
		}
		for _, c := range got {
			if c.Aliases+c.Implications+c.ImpliedBy > 0 {
				return got, candidate, cut, nil
			}
		}
	}
	return clusters, stem, truncated, nil
}

type ptrSearchRow struct {
	Spelling string
	Color    string
	Note     string
}

func (s *Server) ptrSearchRows(clusters []ptrCluster) []ptrSearchRow {
	color := s.contribTagColor()
	rows := make([]ptrSearchRow, 0, len(clusters))
	for _, c := range clusters {
		var parts []string
		if len(c.Matched) > 0 && c.Matched[0] != c.Ideal {
			parts = append(parts, "via "+c.Matched[0])
		}
		if c.Aliases == 1 {
			parts = append(parts, "+1 alias")
		} else if c.Aliases > 1 {
			parts = append(parts, fmt.Sprintf("+%d aliases", c.Aliases))
		}
		if c.Implications > 0 {
			parts = append(parts, fmt.Sprintf("+%d implies", c.Implications))
		}
		if c.ImpliedBy > 0 {
			parts = append(parts, fmt.Sprintf("%d implied by", c.ImpliedBy))
		}
		if len(parts) == 0 {
			parts = append(parts, "no relations")
		}
		rows = append(rows, ptrSearchRow{Spelling: c.Ideal, Color: color(c.Ideal), Note: strings.Join(parts, " · ")})
	}
	return rows
}

// A request with no q at all is the seed the dialog opens on; an empty q
// is a typeahead.
func (s *Server) tagPtrLookupSearch(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.ptrPullOpen, "the Public Tag Repository is unavailable")
	if !ok {
		return
	}
	query := r.URL.Query()
	typed, isTypeahead := query["q"]
	data := map[string]any{"TagID": id, "Seed": !isTypeahead}
	render := func() { s.renderTemplate(w, "partials/tag_ptr_lookup_search.html", data) }

	tagForm := tagFormName(tag.CategoryName, tag.Name)
	q := strings.TrimSpace(strings.Join(typed, ""))
	if isTypeahead && q == "" {
		render()
		return
	}
	// Wider than a single graph call: the seed walks several stems and a
	// substring pass a whole namespace.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	var clusters []ptrCluster
	var truncated bool
	var err error
	stem := q
	if isTypeahead {
		mode := ""
		if query.Get("anywhere") != "" {
			mode = "contains"
		}
		clusters, truncated, err = s.ptrSpellingSearch(ctx, q, mode, ptrSearchLimit)
	} else {
		clusters, stem, truncated, err = s.ptrSeedSearch(ctx, tagForm)
	}
	switch {
	case errors.Is(err, errPTRNoSearch):
		render()
		return
	case errors.Is(err, errPTRSearchUnbounded):
		data["Note"] = "matching anywhere in the name needs a category: prefix."
		render()
		return
	case err != nil:
		data["Note"] = "the Public Tag Repository is unavailable."
		render()
		return
	}

	data["Rows"] = s.ptrSearchRows(clusters)
	switch {
	case isTypeahead:
		if truncated {
			data["Note"] = fmt.Sprintf("showing the first %d.", len(clusters))
		}
	case len(clusters) == 0:
		data["Note"] = "the Public Tag Repository holds no spelling near " + tagForm + "."
	case stem != tagForm:
		data["Note"] = "no PTR spelling starts with " + tagForm + "; showing " + stem + "."
	}
	render()
}

func (s *Server) tagPtrLookupDialog(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.ptrPullOpen, "the Public Tag Repository is unavailable")
	if !ok {
		return
	}
	s.renderTemplate(w, "partials/tag_ptr_lookup_dialog.html", map[string]any{
		"TagID":   id,
		"TagName": tag.Name,
	})
}

func (s *Server) tagPtrLookupPreview(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.ptrPullOpen, "the Public Tag Repository is unavailable")
	if !ok {
		return
	}
	data := map[string]any{"TagID": id, "TagName": tag.Name}
	spelling, ok := s.ptrSpellingForm(r.URL.Query().Get("as"))
	if !ok {
		data["Err"] = "not a tag name"
		s.renderTemplate(w, "partials/tag_ptr_lookup_result.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	graph, err := s.ptrTagLookup(ctx, []string{spelling})
	if err != nil {
		data["Err"] = "the Public Tag Repository is unavailable"
		s.renderTemplate(w, "partials/tag_ptr_lookup_result.html", data)
		return
	}
	data["Spelling"] = spelling
	info := graph[spelling]
	if !info.Known {
		s.renderTemplate(w, "partials/tag_ptr_lookup_result.html", data)
		return
	}
	tagForm := tagFormName(tag.CategoryName, tag.Name)
	data["TagForm"] = tagForm
	groups, err := s.ptrLookupGroups(id, tagForm, spelling, info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	color := s.contribTagColor()
	aliasAdds, implAdds, declared, skipped := 0, 0, 0, 0
	for _, g := range groups {
		for _, row := range g.Rows {
			switch row.Class {
			case "ptr-row-staged":
				if g.Title == "aliases" {
					aliasAdds++
				} else {
					implAdds++
				}
			case "ptr-row-ctx":
				declared++
			default:
				skipped++
			}
		}
	}
	data["Known"] = true
	data["Ideal"] = info.Ideal
	data["SpellingColor"] = color(spelling)
	data["IdealColor"] = color(info.Ideal)
	data["LocalStatus"], data["CanAlias"] = s.ptrLookupLocalStatus(id, tagForm, spelling)
	data["Groups"] = groups
	data["AliasAdds"] = aliasAdds
	data["ImplAdds"] = implAdds
	data["Declared"] = declared
	data["Skipped"] = skipped
	s.renderTemplate(w, "partials/tag_ptr_lookup_result.html", data)
}
