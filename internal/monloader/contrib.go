package monloader

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

func ContribTag(tagSvc func() *tags.Service, id int64) (*models.Tag, bool) {
	tag, err := tagSvc().GetTag(id)
	if err != nil || tag.IsAlias || tag.CategoryName == "rating" {
		return nil, false
	}
	return tag, true
}

func (l *Link) ImageContribPreview(rctx context.Context, tagSvc func() *tags.Service, id int64, sha string) (*ContribPreview, error) {
	_, imageTags, _ := tagSvc().GetImageTags(id)
	ledger, err := tagSvc().TagSourcesForImage(id)
	if err != nil {
		logx.Warnf("TagSourcesForImage: %v", err)
	}
	storage, implied := tags.StorageForms(imageTags, ledger)
	byTag, byAlias := imageTagAliases(tagSvc, imageTags)
	ctx, cancel := context.WithTimeout(rctx, 8*time.Second)
	defer cancel()
	sent, local := l.ptrSubmitSpellings(ctx, storage, byTag)
	preview, err := l.PreviewContrib(ctx, sha, sent, implied)
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
func (l *Link) ptrSubmitSpellings(ctx context.Context, storage []string, byTag map[string][]string) ([]string, map[string]string) {
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
	graph := map[string]PTRTagInfo{}
	for start := 0; start < len(names); start += PTRLookupBatch {
		part, err := l.PTRTagLookup(ctx, names[start:min(start+PTRLookupBatch, len(names))])
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
		if spelling, _, ok := ResolvePTRSpelling(graph, form, byTag[form]); ok && !taken[spelling] {
			sent[i], taken[spelling], local[spelling] = spelling, true, form
		}
	}
	return sent, local
}

// Without this, a PTR spelling pulled here as an alias shows as a petition
// and the operator's own spelling as a new upload of the same tag.
func foldAliasedPTRTags(preview *ContribPreview, byAlias map[string]string) {
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

func imageTagAliases(tagSvc func() *tags.Service, imageTags []models.ImageTag) (byTag map[string][]string, byAlias map[string]string) {
	ids := make([]int64, 0, len(imageTags))
	for _, t := range imageTags {
		ids = append(ids, t.TagID)
	}
	byTag, byAlias, err := tagSvc().AliasForms(ids)
	if err != nil {
		logx.Warnf("AliasesForTagIDs: %v", err)
	}
	return byTag, byAlias
}

// A tag the PTR holds that the ledger does not credit to it is still work
// for a pull, which records the attribution.
func PTRUnattributed(tagSvc func() *tags.Service, id int64, preview *ContribPreview) []string {
	known := map[string]bool{}
	for _, t := range preview.ToAdd {
		if t.Status == "known" {
			known[t.Tag] = true
		}
	}
	if len(known) == 0 {
		return nil
	}
	ledger, err := tagSvc().TagSourcesForImage(id)
	if err != nil {
		return nil
	}
	_, imageTags, _ := tagSvc().GetImageTags(id)
	var out []string
	for _, t := range imageTags {
		name := tags.FormName(t.Category, t.TagName)
		if !known[name] || slices.ContainsFunc(ledger[t.TagID], isPTRSource) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func isPTRSource(src tags.TagSource) bool { return strings.EqualFold(src.Source, "ptr") }

func MappingItems(sha string, adds, petitions []string, reason string) []map[string]any {
	var items []map[string]any
	for _, tag := range adds {
		items = append(items, map[string]any{"kind": "mapping_add", "sha256": sha, "tag": tag})
	}
	for _, tag := range petitions {
		items = append(items, map[string]any{
			"kind": "mapping_petition", "sha256": sha, "tag": tag, "reason": reason,
		})
	}
	return items
}

type ContribReceiptRow struct {
	Label string
	Note  string
}

type ContribReceipt struct {
	Sent, Refused                int
	Adds, Petitions, RefusedRows []ContribReceiptRow
}

// Verdicts pair with items by position, so a count mismatch would
// mislabel them: it answers false.
func ReadContribReceipt(items []map[string]any, results []ContribSendResult) (ContribReceipt, bool) {
	if len(results) != len(items) {
		return ContribReceipt{}, false
	}
	var r ContribReceipt
	for _, res := range results {
		if res.Result == "staged" {
			r.Sent++
		} else {
			r.Refused++
		}
	}
	r.Adds, r.Petitions, r.RefusedRows = receiptRows(items, results)
	return r, true
}

func receiptRows(items []map[string]any, results []ContribSendResult) (adds, petitions, refused []ContribReceiptRow) {
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
		row := ContribReceiptRow{Label: label}
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

// A and B follow hydrus: sibling A=bad (alias), B=good; parent A=child
// (the carrying tag), B=parent (the implied tag).
type TagPairRow struct {
	Kind           string
	A, B           string
	AColor, BColor string
	Rel            string
	Direction      string
	Note           string
}

// Space delimits the fields because the tag charset excludes whitespace,
// unlike '|'.
func (p TagPairRow) Value() string { return p.Kind + " " + p.A + " " + p.B }

func (p TagPairRow) Label() string {
	if p.Kind == "sibling" {
		return p.A + " -> " + p.B
	}
	return p.A + " => " + p.B
}

type TagContribDiff struct {
	Local, PTROnly []TagPairRow
	Pullable       bool
	Provisional    bool
	KnownAs        string
	Unknown        bool
	Empty          bool
	IdealElsewhere string
}

func (l *Link) tagContribRows(ctx context.Context, tagSvc func() *tags.Service, id int64, tag *models.Tag) (*TagContribDiff, error) {
	tagForm := tags.FormName(tag.CategoryName, tag.Name)
	aliases, err := tagSvc().AliasesForTagIDs([]int64{id})
	if err != nil {
		return nil, err
	}
	implications, err := tagSvc().ListImplications(id)
	if err != nil {
		return nil, err
	}
	impliedBy, err := tagSvc().ImpliedBy(id)
	if err != nil {
		return nil, err
	}
	d := &TagContribDiff{}
	localAlias, localImplied := map[string]bool{}, map[string]bool{}
	var aliasForms []string
	for _, a := range aliases[id] {
		form := tags.FormName(a.CategoryName, a.Name)
		d.Local = append(d.Local, TagPairRow{Kind: "sibling", A: form, B: tagForm, Rel: "alias of this"})
		localAlias[form] = true
		aliasForms = append(aliasForms, form)
	}
	for _, im := range impliedBy {
		d.Local = append(d.Local, TagPairRow{Kind: "parent", A: tags.FormName(im.ParentCategoryName, im.ParentName), B: tagForm, Rel: "implied by"})
	}
	for _, im := range implications {
		d.Local = append(d.Local, TagPairRow{Kind: "parent", A: tagForm, B: tags.FormName(im.ImpliedCategoryName, im.ImpliedName), Rel: "implies"})
		localImplied[tags.FormName(im.ImpliedCategoryName, im.ImpliedName)] = true
	}
	names := append([]string{tagForm}, aliasForms...)
	names = names[:min(len(names), PTRLookupBatch)]
	graph, err := l.PTRTagLookup(ctx, names)
	if err != nil {
		return nil, err
	}
	known, info, ok := ResolvePTRSpelling(graph, tagForm, aliasForms)
	if ok && known != tagForm {
		d.KnownAs = known
	}
	var toPreview []*TagPairRow
	for i := range d.Local {
		// Known only under another spelling, pair-preview reads every
		// local relation as new; those the cluster already carries would
		// propose flipping the ideal, so they fold as covered.
		if d.KnownAs != "" && heldByCluster(tagSvc, d.Local[i], tagForm, known, info) {
			d.Local[i].Direction = "covered"
			continue
		}
		toPreview = append(toPreview, &d.Local[i])
	}
	previews, err := l.pairPreviews(ctx, toPreview)
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
	d.Pullable = pullWouldAdd(tagSvc, id, info, known)
	d.Empty = len(info.Aliases) == 0 && len(info.Implications) == 0 && len(info.ImpliedBy) == 0 &&
		(info.Ideal == "" || info.Ideal == known)
	// Only pairs the index holds (petition or pending) qualify; the rest
	// is sibling-chain noise. Rows are keyed on the spelling the PTR
	// answered for, the name the index holds them under.
	var candidates []TagPairRow
	for _, a := range info.Aliases {
		if a == "" || a == known || localAlias[localForm(tagSvc, a)] {
			continue
		}
		candidates = append(candidates, TagPairRow{Kind: "sibling", A: a, B: known, Rel: "alias of this"})
	}
	// The ideal is never a petition row: a pull adopts it as an alias
	// here, or leaves it alone when another tag here holds the name.
	if info.Ideal != "" && info.Ideal != known && !localAlias[localForm(tagSvc, info.Ideal)] && tagSvc().FormExists(info.Ideal) {
		d.IdealElsewhere = info.Ideal
	}
	for _, im := range info.Implications {
		if im == "" || im == known || localImplied[localForm(tagSvc, im)] {
			continue
		}
		// The pull skips an endpoint that is an alias here, so the edge
		// has no local form to disagree with.
		if row, ok := tagSvc().FormRow(im); ok && row.IsAlias {
			continue
		}
		candidates = append(candidates, TagPairRow{Kind: "parent", A: known, B: im, Rel: "implies"})
	}
	toPreview = toPreview[:0]
	for i := range candidates {
		// A petition needs a local judgement, which an endpoint the
		// catalog has never seen cannot give.
		other := candidates[i].A
		if other == known {
			other = candidates[i].B
		}
		if tagSvc().FormExists(other) {
			toPreview = append(toPreview, &candidates[i])
		}
	}
	previews, err = l.pairPreviews(ctx, toPreview)
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
func (l *Link) pairPreviews(ctx context.Context, rows []*TagPairRow) ([]*PairPreview, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([]*PairPreview, len(rows))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var failed sync.Once
	var firstErr error
	for i, row := range rows {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			preview, err := l.PreviewPair(ctx, row.Kind, row.A, row.B)
			if err != nil {
				failed.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			out[i] = preview
		})
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, ctx.Err()
}

func localForm(tagSvc func() *tags.Service, name string) string {
	if form, ok := tagSvc().NormalizeForm(name); ok {
		return form
	}
	return name
}

func heldByCluster(tagSvc func() *tags.Service, row TagPairRow, tagForm, known string, info PTRTagInfo) bool {
	names := func(list []string) bool {
		return slices.ContainsFunc(list, func(n string) bool { return localForm(tagSvc, n) == row.A })
	}
	switch {
	case row.Kind == "sibling":
		return row.A == known || row.A == localForm(tagSvc, info.Ideal) || names(info.Aliases)
	case row.A == tagForm:
		return slices.ContainsFunc(info.Implications, func(n string) bool { return localForm(tagSvc, n) == row.B })
	default:
		return names(info.ImpliedBy)
	}
}

// Uses applyPTRTagInfo's normalization and row lookups: comparing
// spellings as strings would keep offering a pull that adopts nothing.
func pullWouldAdd(tagSvc func() *tags.Service, tagID int64, info PTRTagInfo, tagForm string) bool {
	svc := tagSvc()
	free := func(n string) bool { return n != "" && n != tagForm && svc.FormFree(n) }
	return slices.ContainsFunc(info.Aliases, free) || free(info.Ideal) ||
		slices.ContainsFunc(info.Implications, func(im string) bool {
			return im != "" && im != tagForm && svc.WouldAddImplication(tagID, im, true)
		}) ||
		slices.ContainsFunc(info.ImpliedBy, func(im string) bool {
			return im != "" && im != tagForm && svc.WouldAddImplication(tagID, im, false)
		})
}

func (l *Link) TagContribPreview(rctx context.Context, tagSvc func() *tags.Service, id int64, tag *models.Tag) (*TagContribDiff, error) {
	ctx, cancel := context.WithTimeout(rctx, 8*time.Second)
	defer cancel()
	return l.tagContribRows(ctx, tagSvc, id, tag)
}

// Each value is a TagPairRow's Value: "kind a b".
func PairItems(values []string, direction, reason string) []map[string]any {
	var items []map[string]any
	for _, v := range values {
		parts := strings.SplitN(v, " ", 3)
		if len(parts) != 3 {
			continue
		}
		if item := pairSendItem(parts[0], direction, parts[1], parts[2], reason); item != nil {
			items = append(items, item)
		}
	}
	return items
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
