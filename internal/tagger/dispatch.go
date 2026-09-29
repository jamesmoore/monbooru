package tagger

import (
	"github.com/monbooru/monbooru/internal/logx"
)

type DispatchTable struct {
	rules map[string]DispatchRule
}

// An empty Name keeps the source label as the tag name.
type DispatchRule struct {
	Drop    bool
	CatID   int64
	CatName string
	Name    string
}

// An overlay rule that fails to compile leaves the embedded rule for its
// source in place.
func LoadDispatch(modelPath, taggerName string, catIDs map[string]int64) *DispatchTable {
	out := &DispatchTable{rules: map[string]DispatchRule{}}
	for _, r := range EmbeddedDispatchRules(taggerName) {
		if rule, ok := compileDispatchRule(r, catIDs); ok {
			out.rules[r.Source] = rule
		}
	}
	for _, r := range OverlayDispatchRules(modelPath, taggerName) {
		if rule, ok := compileDispatchRule(r, catIDs); ok {
			out.rules[r.Source] = rule
		}
	}
	return out
}

func compileDispatchRule(r DispatchEntry, catIDs map[string]int64) (DispatchRule, bool) {
	rule := DispatchRule{}
	if r.Category == "" {
		rule.Drop = true
	} else {
		cid, ok := catIDs[r.Category]
		if !ok {
			logx.Debugf("tagger: dispatch %q drops unknown target category %q", r.Source, r.Category)
			return DispatchRule{}, false
		}
		rule.CatID = cid
		rule.CatName = r.Category
	}
	if r.Name != "" {
		name, ok := sanitizeLabel(r.Name, 0)
		if !ok {
			logx.Debugf("tagger: dispatch %q drops unsupported target name %q", r.Source, r.Name)
			return DispatchRule{}, false
		}
		rule.Name = name
	}
	return rule, true
}

func (d *DispatchTable) Lookup(source string) (DispatchRule, bool) {
	if d == nil {
		return DispatchRule{}, false
	}
	r, ok := d.rules[source]
	return r, ok
}
