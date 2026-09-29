package web

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
)

// Install snippets are built in Go so the template never does shell quoting.
type taggerRow struct {
	Name                string
	Description         string
	Available           bool
	Reason              string
	Enabled             bool
	ConfidenceThreshold float64
	ConfigSummary       string
	Differs             bool
	Installed           bool
	Supported           bool
	Gated               bool
	HostCommand         string
	DockerCommand       string
	// The browser fetches these itself, which keeps the server's
	// no-outbound-request promise.
	Files     []tagger.CatalogFile
	TargetDir string
}

func (s *Server) installedTaggerRow(t tagger.TaggerStatus, totalGalleries int, modelPath string) taggerRow {
	ruleCount := tagger.OverlayRuleCount(modelPath, t.Name)
	return taggerRow{
		Name:                t.Name,
		Available:           t.Available,
		Reason:              t.Reason,
		Enabled:             t.Enabled,
		ConfidenceThreshold: t.ConfidenceThreshold,
		ConfigSummary:       taggerConfigSummary(t.TaggerInstance, totalGalleries, ruleCount),
		Differs:             taggerDiffersFromStock(t.TaggerInstance, modelPath, ruleCount),
		Installed:           true,
	}
}

func taggerConfigSummary(inst config.TaggerInstance, totalGalleries, ruleCount int) string {
	out := taggerThresholdSummary(inst.ConfidenceThreshold, inst.CategoryThresholds, inst.DisabledCategories)
	if inst.Galleries != nil && len(inst.Galleries) != totalGalleries {
		if len(inst.Galleries) == 0 {
			out += ", no galleries"
		} else {
			out += ", galleries: " + strings.Join(inst.Galleries, ", ")
		}
	}
	switch {
	case ruleCount == 1:
		out += ", 1 rule"
	case ruleCount > 1:
		out += fmt.Sprintf(", %d rules", ruleCount)
	}
	return out
}

func taggerDiffersFromStock(inst config.TaggerInstance, modelPath string, ruleCount int) bool {
	seed := tagger.SeedTaggerInstance(inst.Name, inst.Enabled, catalogEntryByName(modelPath, inst.Name))
	if inst.ConfidenceThreshold != seed.ConfidenceThreshold {
		return true
	}
	if !maps.Equal(inst.CategoryThresholds, seed.CategoryThresholds) {
		return true
	}
	if !maps.Equal(inst.PerCategoryTopK, seed.PerCategoryTopK) {
		return true
	}
	a := append([]string(nil), inst.DisabledCategories...)
	b := append([]string(nil), seed.DisabledCategories...)
	sort.Strings(a)
	sort.Strings(b)
	if !slices.Equal(a, b) {
		return true
	}
	return inst.Galleries != nil || ruleCount > 0
}

type thresholdRow struct {
	Category         string
	Override         string
	MaxTags          string
	MaxDefault       int
	Color            string
	Disabled         bool
	ViaRules         bool // reached only through dispatch rules, not the model
	DefaultThreshold string
	DefaultMaxTags   string
}

type taggerGalleryRow struct {
	Name    string
	Checked bool
}

func (s *Server) settingsTaggerPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	newProvider := strings.ToLower(strings.TrimSpace(r.FormValue("execution_provider")))
	newProvider = cmp.Or(newProvider, "cpu")
	if !config.IsValidExecutionProvider(newProvider) {
		writeInlineFlash(w, "err", "Invalid execution provider: "+newProvider)
		return
	}
	// ORT env init is not re-entrant, so no probe while a job runs.
	if newProvider != s.executionProvider() {
		if s.jobs.IsRunning() {
			writeInlineFlash(w, "err", "A job is running; try again when it finishes.")
			return
		}
		if newProvider != "cpu" {
			// SA4023: the stub build's probe always errors, the tagger build's
			// does not, and staticcheck only ever sees the stub.
			if err := tagger.CheckProviderAvailable(newProvider); err != nil { //nolint:staticcheck
				writeInlineFlash(w, "err", "Cannot enable "+newProvider+": "+err.Error())
				return
			}
		}
	}

	s.cfgMu.Lock()
	providerChanged := s.cfg.Tagger.ExecutionProvider != newProvider
	s.cfg.Tagger.ExecutionProvider = newProvider
	if n, err := strconv.Atoi(r.FormValue("parallel")); err == nil && n >= 1 {
		s.cfg.Tagger.Parallel = n
	}
	if v := r.FormValue("idle_release_after_minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			s.cfg.Tagger.IdleReleaseAfterMinutes = n
		}
	}
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	// The old provider's session would otherwise hold its memory until
	// the idle release.
	if providerChanged {
		tagger.ReleaseAll()
	}
	logx.Infof("settings: tagger updated (execution_provider=%s)", newProvider)
	writeInlineFlash(w, "ok", "Saved.")
	s.renderTemplate(w, "partials/tagger_mode_badge.html", map[string]any{
		"Provider": newProvider,
		"OOB":      true,
	})
}

func (s *Server) settingsTaggerEnablePost(w http.ResponseWriter, r *http.Request) {
	s.applyTaggerEnabled(w, strings.TrimSpace(r.PathValue("name")), true)
}

func (s *Server) settingsTaggerDisablePost(w http.ResponseWriter, r *http.Request) {
	s.applyTaggerEnabled(w, strings.TrimSpace(r.PathValue("name")), false)
}

func (s *Server) updateTagger(name string, mutate func(*config.TaggerInstance)) error {
	modelPath := s.modelPath()
	s.cfgMu.Lock()
	found := false
	for i := range s.cfg.Tagger.Taggers {
		if s.cfg.Tagger.Taggers[i].Name == name {
			mutate(&s.cfg.Tagger.Taggers[i])
			found = true
			break
		}
	}
	if !found {
		catalog := catalogEntryByName(modelPath, name)
		seeded := tagger.SeedTaggerInstance(name, false, catalog)
		mutate(&seeded)
		s.cfg.Tagger.Taggers = append(s.cfg.Tagger.Taggers, seeded)
	}
	s.cfgMu.Unlock()
	return s.saveConfig()
}

func (s *Server) applyTaggerEnabled(w http.ResponseWriter, name string, enabled bool) {
	if err := tagger.ValidateTaggerName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.Enabled = enabled
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	logx.Infof("settings: tagger %q %s", name, verb)
	// A header flash: the refresh discards an inline body before it paints.
	setFlashHeader(w, "Tagger "+name+" "+verb+".", "ok", nil)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) settingsTaggerConfigGet(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r)
	if !ok {
		return
	}
	rows, global, ok := s.thresholdDialogData(name)
	if !ok {
		http.Error(w, "tagger not found", http.StatusNotFound)
		return
	}
	galRows, allChecked, _ := s.galleryDialogData(name)
	inst, _ := s.resolveTaggerInstance(name)
	modelPath := s.modelPath()
	catIDs, catList := s.categoryChoices()
	labelCount := 0
	if views, err := tagger.BrowseLabels(modelPath, name, taggerTagsFile(inst), catIDs); err == nil {
		labelCount = len(views)
	}

	embedded := tagger.EmbeddedDispatchRules(name)
	overlay := tagger.OverlayDispatchRules(modelPath, name)
	merged := tagger.MergedDispatchRules(modelPath, name)
	exportJSON := ""
	if b, err := tagger.MarshalDispatchDoc(merged); err == nil {
		exportJSON = string(b) + "\n"
	}
	profileJSON, profileLines := "", []exportLine(nil)
	sidecarFields := tagger.SidecarProfileFields(modelPath, name)
	if profile, err := tagger.ResolveProfile(modelPath, name, taggerTagsFile(inst)); err == nil {
		if b, err := tagger.ProfileExportDoc(profile); err == nil {
			profileJSON = string(b) + "\n"
			marked := map[string]bool{}
			for _, f := range sidecarFields {
				marked[f] = true
			}
			for _, line := range strings.Split(string(b), "\n") {
				mark := ""
				trimmed := strings.TrimSpace(line)
				if i := strings.Index(trimmed, `":`); strings.HasPrefix(trimmed, `"`) && i > 0 && marked[trimmed[1:i]] {
					mark = "add"
				}
				profileLines = append(profileLines, exportLine{Text: line, Mark: mark})
			}
		}
	}

	csrf := s.csrfToken(sessionFromContext(r.Context()))
	s.renderTemplate(w, "partials/tagger_config_dialog.html", map[string]any{
		"Name":         name,
		"Global":       global,
		"Rows":         rows,
		"GalRows":      galRows,
		"AllChecked":   allChecked,
		"Categories":   catList,
		"LabelCount":   labelCount,
		"CustomRules":  len(overlay),
		"ExportRules":  exportRuleLines(embedded, overlay),
		"ExportJSON":   exportJSON,
		"RuleCount":    len(merged),
		"ProfileLines": profileLines,
		"ProfileJSON":  profileJSON,
		"ProfileStock": len(sidecarFields) == 0,
		"CSRFToken":    csrf,
	})
}

type exportLine struct {
	Text string
	Mark string
}

func exportRuleLines(embedded, overlay []tagger.DispatchEntry) []exportLine {
	embByte := map[string]tagger.DispatchEntry{}
	for _, e := range embedded {
		embByte[e.Source] = e
	}
	ovr := map[string]tagger.DispatchEntry{}
	for _, e := range overlay {
		ovr[e.Source] = e
	}
	sources := make([]string, 0, len(embByte)+len(ovr))
	seen := map[string]bool{}
	for _, e := range embedded {
		if !seen[e.Source] {
			seen[e.Source] = true
			sources = append(sources, e.Source)
		}
	}
	for _, e := range overlay {
		if !seen[e.Source] {
			seen[e.Source] = true
			sources = append(sources, e.Source)
		}
	}
	sort.Strings(sources)

	ruleText := func(e tagger.DispatchEntry) string {
		b, _ := json.Marshal(e)
		return "    " + string(b) + ","
	}
	lines := []exportLine{{Text: "{"}, {Text: `  "version": 1,`}, {Text: `  "rules": [`}}
	var run []tagger.DispatchEntry
	flushRun := func() {
		if len(run) <= 5 {
			for _, e := range run {
				lines = append(lines, exportLine{Text: ruleText(e)})
			}
		} else {
			lines = append(lines, exportLine{Text: fmt.Sprintf("    ... %d default rules ...", len(run)), Mark: "gap"})
		}
		run = nil
	}
	for _, src := range sources {
		o, hasOverlay := ovr[src]
		e, hasEmbedded := embByte[src]
		if !hasOverlay {
			run = append(run, e)
			continue
		}
		flushRun()
		if hasEmbedded {
			lines = append(lines, exportLine{Text: ruleText(e), Mark: "del"})
		}
		lines = append(lines, exportLine{Text: ruleText(o), Mark: "add"})
	}
	flushRun()
	lines = append(lines, exportLine{Text: "  ]"}, exportLine{Text: "}"})
	return lines
}

type taggerLabelRow struct {
	Source      string
	CatName     string
	TagName     string
	Color       string
	Muted       bool
	CategoryOff bool
	Rule        string
}

// Growing stops at taggerLabelMaxRows: past it the table costs more to
// render than a search costs to type.
const (
	taggerLabelPageSize = 50
	taggerLabelMaxRows  = 500
)

func (s *Server) settingsTaggerLabelsGet(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r)
	if !ok {
		return
	}
	s.renderTaggerLabels(w, r, name)
}

func (s *Server) renderTaggerLabels(w http.ResponseWriter, r *http.Request, name string) {
	inst, ok := s.resolveTaggerInstance(name)
	if !ok {
		http.Error(w, "tagger not found", http.StatusNotFound)
		return
	}
	modelPath := s.modelPath()
	catIDs, _ := s.categoryChoices()
	colors := s.categoryColors()
	views, err := tagger.BrowseLabels(modelPath, name, taggerTagsFile(inst), catIDs)
	custom := len(tagger.OverlayDispatchRules(modelPath, name))
	if err != nil {
		s.renderTemplate(w, "partials/tagger_labels_rows.html", map[string]any{
			"Name":        name,
			"CustomRules": custom,
			"Err":         "Cannot read the label file: " + err.Error(),
		})
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.FormValue("q")))
	filter := r.FormValue("filter")
	limit := taggerLabelPageSize
	if n, err := strconv.Atoi(r.FormValue("limit")); err == nil && n > limit {
		limit = min(n, taggerLabelMaxRows)
	}
	disabled := map[string]bool{}
	for _, cat := range inst.DisabledCategories {
		disabled[cat] = true
	}
	var rows []taggerLabelRow
	more := 0
	for _, v := range views {
		if q != "" && !strings.Contains(strings.ToLower(v.Source), q) && !strings.Contains(strings.ToLower(v.TagName), q) {
			continue
		}
		catOff := !v.Muted && disabled[v.CatName]
		switch filter {
		case "customized":
			if v.Rule != "custom" {
				continue
			}
		case "muted":
			if !v.Muted && !catOff {
				continue
			}
		}
		if len(rows) >= limit {
			more++
			continue
		}
		rows = append(rows, taggerLabelRow{
			Source:      v.Source,
			CatName:     v.CatName,
			TagName:     v.TagName,
			Color:       colors[v.CatName],
			Muted:       v.Muted,
			CategoryOff: catOff,
			Rule:        v.Rule,
		})
	}
	s.renderTemplate(w, "partials/tagger_labels_rows.html", map[string]any{
		"Name":        name,
		"Rows":        rows,
		"More":        more,
		"NextLimit":   limit + taggerLabelPageSize,
		"CanGrow":     limit < taggerLabelMaxRows,
		"CustomRules": custom,
	})
}

func taggerTagsFile(inst config.TaggerInstance) string {
	if inst.TagsFile != "" {
		return inst.TagsFile
	}
	return tagger.DefaultTagsFile
}

func (s *Server) categoryChoices() (map[string]int64, []string) {
	ids := map[string]int64{}
	var names []string
	cats, err := s.tagSvc().ListCategories()
	if err != nil {
		return ids, names
	}
	for _, c := range cats {
		ids[c.Name] = c.ID
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return ids, names
}

// Every row posts its category, so an absent Enable box means muted.
func parseThresholdForm(r *http.Request) (global float64, overrides map[string]float64, topK map[string]int, disabled []string, errMsg string) {
	globalRaw := strings.TrimSpace(r.FormValue("global_threshold"))
	global, err := strconv.ParseFloat(globalRaw, 64)
	if err != nil || global < 0 || global > 1 {
		return 0, nil, nil, nil, "Global threshold must be between 0 and 1."
	}
	overrides = map[string]float64{}
	topK = map[string]int{}
	for _, cat := range r.Form["category"] {
		cat = strings.TrimSpace(cat)
		if cat == "" {
			continue
		}
		raw := strings.TrimSpace(r.FormValue("threshold_" + cat))
		if raw != "" {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil || v < 0 || v > 1 {
				return 0, nil, nil, nil, "Threshold for " + cat + " must be between 0 and 1."
			}
			overrides[cat] = v
		}
		rawK := strings.TrimSpace(r.FormValue("maxtags_" + cat))
		if rawK != "" {
			n, err := strconv.Atoi(rawK)
			if err != nil || n < 0 {
				return 0, nil, nil, nil, "Max tags for " + cat + " must be 0 or higher."
			}
			topK[cat] = n
		}
		if r.FormValue("enable_"+cat) == "" {
			disabled = append(disabled, cat)
		}
	}
	return global, overrides, topK, disabled, ""
}

// nil means every gallery; a non-nil empty slice means none and must stay
// non-nil so the TOML keeps galleries = [].
func (s *Server) parseGalleriesForm(r *http.Request) []string {
	if r.FormValue("all") == "on" {
		return nil
	}
	galleries := []string{}
	valid := map[string]bool{}
	s.cfgMu.Lock()
	for _, g := range s.cfg.Galleries {
		valid[g.Name] = true
	}
	s.cfgMu.Unlock()
	for _, n := range r.Form["gallery_names"] {
		n = strings.TrimSpace(n)
		if n == "" || !valid[n] {
			continue
		}
		galleries = append(galleries, n)
	}
	return galleries
}

func (s *Server) settingsTaggerMappingPost(w http.ResponseWriter, r *http.Request) {
	name, ok := taggerNameAndForm(w, r)
	if !ok {
		return
	}
	modelPath := s.modelPath()
	if errMsg := s.applyMappingRule(name, modelPath, r); errMsg != "" {
		w.Header().Set("HX-Retarget", "#flash-tagger-config-"+name)
		writeInlineFlash(w, "err", errMsg)
		return
	}
	writeFlashOOB(w, "flash-tagger-config-"+name, "", "")
	s.renderTaggerLabels(w, r, name)
}

func (s *Server) applyMappingRule(name, modelPath string, r *http.Request) (errMsg string) {
	source := strings.TrimSpace(r.FormValue("rule_source"))
	if source == "" {
		return "Malformed mapping rule."
	}
	// Validated before the overlay lock, so the locked section is only
	// read, swap, write.
	reset := r.FormValue("rule_reset") != ""
	entry := tagger.DispatchEntry{Source: source}
	if !reset && r.FormValue("rule_mute") == "" {
		category := r.FormValue("rule_category")
		catIDs, _ := s.categoryChoices()
		if _, ok := catIDs[category]; !ok {
			return "Unknown category " + category + " for label " + source + "."
		}
		rename := ""
		if n := strings.TrimSpace(r.FormValue("rule_name")); n != "" && n != source {
			valid, err := tags.ValidateTagName(n)
			if err != nil {
				return "Invalid rename for label " + source + ": " + err.Error()
			}
			rename = valid
		}
		if target := cmp.Or(rename, source); category == "rating" && !tags.IsCanonicalRating(target) {
			return "A rating label must become general, sensitive, questionable or explicit, not " + target + "."
		}
		entry = tagger.DispatchEntry{Source: source, Category: category, Name: rename}
	}
	if err := tagger.UpdateDispatchOverlay(modelPath, name, func(overlay map[string]tagger.DispatchEntry) error {
		if reset {
			delete(overlay, source)
		} else {
			overlay[source] = entry
		}
		return nil
	}); err != nil {
		return "Could not save dispatch.json: " + err.Error()
	}
	logx.Infof("settings: tagger %q mapping rule for %q updated", name, source)
	return ""
}

func (s *Server) settingsTaggerConfigPost(w http.ResponseWriter, r *http.Request) {
	name, ok := taggerNameAndForm(w, r)
	if !ok {
		return
	}
	global, overrides, topK, disabled, errMsg := parseThresholdForm(r)
	if errMsg != "" {
		writeInlineFlash(w, "err", errMsg)
		return
	}
	galleries := s.parseGalleriesForm(r)
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.ConfidenceThreshold = global
		if len(overrides) > 0 {
			t.CategoryThresholds = overrides
		} else {
			t.CategoryThresholds = nil
		}
		if len(topK) > 0 {
			t.PerCategoryTopK = topK
		} else {
			t.PerCategoryTopK = nil
		}
		t.DisabledCategories = disabled
		t.Galleries = galleries
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: tagger %q config updated (global=%.2f, %d threshold overrides, %d top-K overrides, %d disabled, all_galleries=%t)",
		name, global, len(overrides), len(topK), len(disabled), galleries == nil)
	setFlashHeader(w, "Tagger "+name+" configuration saved.", "ok", nil)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) settingsTaggerResetPost(w http.ResponseWriter, r *http.Request) {
	name, ok := taggerNameAndForm(w, r)
	if !ok {
		return
	}
	defaults := tagger.SeedTaggerInstance(name, false, catalogEntryByName(s.modelPath(), name))
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.ConfidenceThreshold = defaults.ConfidenceThreshold
		t.CategoryThresholds = defaults.CategoryThresholds
		t.PerCategoryTopK = defaults.PerCategoryTopK
		t.DisabledCategories = defaults.DisabledCategories
		t.Galleries = nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	overlay := filepath.Join(s.modelPath(), name, "dispatch.json")
	if err := os.Remove(overlay); err != nil && !os.IsNotExist(err) {
		logx.Warnf("reset tagger %q: remove %q: %v", name, overlay, err)
		writeInlineFlash(w, "err", "Reset saved but could not delete dispatch.json: "+err.Error())
		return
	}
	logx.Infof("settings: tagger %q reset to stock", name)
	setFlashHeader(w, "Tagger "+name+" reset to stock.", "ok", nil)
	w.Header().Set("HX-Refresh", "true")
}

// Through discovery: only it fills ModelFile and TagsFile from disk, and
// a seeded entry assumes tags.csv, which some taggers do not ship.
func (s *Server) resolveTaggerInstance(name string) (config.TaggerInstance, bool) {
	for _, t := range tagger.DiscoverTaggers(s.cfgSnapshot()) {
		if t.Name == name {
			return t.TaggerInstance, true
		}
	}
	return config.TaggerInstance{}, false
}

// Every gallery category is listed, so one no tagger reaches yet can be
// tuned before a rule routes into it.
func (s *Server) thresholdDialogData(name string) (rows []thresholdRow, global float64, ok bool) {
	inst, ok := s.resolveTaggerInstance(name)
	if !ok {
		return nil, 0, false
	}
	modelPath := s.modelPath()
	global = inst.ConfidenceThreshold

	profile, _ := tagger.ResolveProfile(modelPath, name, taggerTagsFile(inst))
	emit := profile.EmittedCategories()

	colors := s.categoryColors()

	// Seeded as the tagger Reset seeds, so a category's Reset lands on
	// the same values.
	defaults := tagger.SeedTaggerInstance(name, false, catalogEntryByName(modelPath, name))

	seen := map[string]bool{}
	appendRow := func(cat string, viaRules bool) {
		if seen[cat] {
			return
		}
		seen[cat] = true
		rows = append(rows, thresholdRow{
			Category:         cat,
			Override:         formatOverride(inst.CategoryThresholds, cat),
			MaxTags:          formatTopKOverride(inst.PerCategoryTopK, cat),
			MaxDefault:       tagger.ResolveTopK(nil, cat),
			Color:            colors[cat],
			Disabled:         slices.Contains(inst.DisabledCategories, cat),
			ViaRules:         viaRules,
			DefaultThreshold: formatOverride(defaults.CategoryThresholds, cat),
			DefaultMaxTags:   formatTopKOverride(defaults.PerCategoryTopK, cat),
		})
	}
	appendSorted := func(cats []string, viaRules bool) {
		cats = append([]string(nil), cats...)
		sort.Strings(cats)
		for _, cat := range cats {
			appendRow(cat, viaRules)
		}
	}
	appendSorted(emit, false)
	var dispatchTargets []string
	for _, cat := range tagger.DispatchTargetCategories(modelPath, name) {
		if _, ok := colors[cat]; ok {
			dispatchTargets = append(dispatchTargets, cat)
		}
	}
	appendSorted(dispatchTargets, true)
	var rest []string
	for cat := range colors {
		rest = append(rest, cat)
	}
	appendSorted(rest, false)
	// Overrides for categories the gallery no longer has still render, so
	// they can be cleared.
	var stale []string
	for cat := range inst.CategoryThresholds {
		stale = append(stale, cat)
	}
	for cat := range inst.PerCategoryTopK {
		stale = append(stale, cat)
	}
	stale = append(stale, inst.DisabledCategories...)
	appendSorted(stale, false)
	return rows, global, true
}

func formatOverride(m map[string]float64, key string) string {
	if v, ok := m[key]; ok {
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	return ""
}

// An explicit zero stays "0": it is the operator's opt-out of the cap.
func formatTopKOverride(m map[string]int, key string) string {
	if v, ok := m[key]; ok {
		return strconv.Itoa(v)
	}
	return ""
}

func taggerThresholdSummary(global float64, overrides map[string]float64, disabled []string) string {
	out := fmt.Sprintf("global %.2f", global)
	for _, k := range slices.Sorted(maps.Keys(overrides)) {
		out += fmt.Sprintf(", %s %.2f", k, overrides[k])
	}
	if len(disabled) > 0 {
		d := append([]string(nil), disabled...)
		sort.Strings(d)
		out += " (disabled: " + strings.Join(d, ", ") + ")"
	}
	return out
}

func (s *Server) galleryDialogData(name string) (rows []taggerGalleryRow, allChecked bool, ok bool) {
	inst, ok := s.resolveTaggerInstance(name)
	if !ok {
		return nil, false, false
	}
	s.cfgMu.Lock()
	galleries := append([]config.Gallery(nil), s.cfg.Galleries...)
	s.cfgMu.Unlock()
	allChecked = inst.Galleries == nil
	picked := map[string]bool{}
	for _, n := range inst.Galleries {
		picked[n] = true
	}
	for _, g := range galleries {
		rows = append(rows, taggerGalleryRow{
			Name:    g.Name,
			Checked: allChecked || picked[g.Name],
		})
	}
	return rows, allChecked, true
}

func catalogEntryByName(modelPath, name string) *tagger.CatalogEntry {
	for _, e := range tagger.LoadCatalog(modelPath) {
		if e.Name == name {
			entry := e
			return &entry
		}
	}
	return nil
}

// Persisted, so a re-downloaded model has to be re-enabled deliberately.
func (s *Server) disableUnavailableTaggers() {
	available := map[string]bool{}
	for _, t := range tagger.DiscoverTaggers(s.cfgSnapshot()) {
		available[t.Name] = t.Available
	}
	s.cfgMu.Lock()
	changed := false
	for i, t := range s.cfg.Tagger.Taggers {
		if t.Enabled && !available[t.Name] {
			s.cfg.Tagger.Taggers[i].Enabled = false
			changed = true
			logx.Infof("settings: auto-disabled tagger %q (files missing)", t.Name)
		}
	}
	s.cfgMu.Unlock()
	if changed {
		if err := s.saveConfig(); err != nil {
			logx.Warnf("auto-disable taggers: save config: %v", err)
		}
	}
}

func (s *Server) persistNewlyDiscoveredTaggers() {
	discovered := tagger.DiscoverTaggers(s.cfgSnapshot())
	modelPath := s.modelPath()
	s.cfgMu.Lock()
	known := make(map[string]bool, len(s.cfg.Tagger.Taggers))
	for _, t := range s.cfg.Tagger.Taggers {
		known[t.Name] = true
	}
	added := false
	for _, d := range discovered {
		if known[d.Name] || !d.Available {
			continue
		}
		s.cfg.Tagger.Taggers = append(s.cfg.Tagger.Taggers,
			tagger.SeedTaggerInstance(d.Name, true, catalogEntryByName(modelPath, d.Name)))
		known[d.Name] = true
		added = true
		logx.Infof("settings: auto-enabled discovered tagger %q", d.Name)
	}
	s.cfgMu.Unlock()
	if added {
		if err := s.saveConfig(); err != nil {
			logx.Warnf("auto-enable taggers: save config: %v", err)
		}
	}
}

func (s *Server) settingsTaggerDeletePost(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r)
	if !ok {
		return
	}
	s.cfgMu.Lock()
	for _, t := range s.cfg.Tagger.Taggers {
		if t.Name == name && t.Enabled {
			s.cfgMu.Unlock()
			writeInlineFlash(w, "err", "Disable tagger "+name+" before deleting it.")
			return
		}
	}
	// Not through modelPath: it read-locks cfgMu, which deadlocks under
	// the write lock held here.
	dir := filepath.Join(s.cfg.Paths.ModelPath, name)
	s.cfgMu.Unlock()
	// The folder goes first: an entry dropped before a failed removal
	// leaves memory and the TOML disagreeing.
	if err := os.RemoveAll(dir); err != nil {
		logx.Warnf("delete tagger %q: remove %q: %v", name, dir, err)
		writeInlineFlash(w, "err", "Could not delete the tagger folder: "+err.Error())
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		c.Tagger.Taggers = slices.DeleteFunc(c.Tagger.Taggers, func(t config.TaggerInstance) bool { return t.Name == name })
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: tagger %q deleted (folder %s removed)", name, dir)
	w.Header().Set("HX-Refresh", "true")
	writeInlineFlash(w, "ok", "Tagger "+name+" deleted.")
}
