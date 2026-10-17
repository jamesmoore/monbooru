package tagger

import (
	"cmp"

	"github.com/monbooru/monbooru/internal/tags"
)

// Category 9, WD14's rating family, is left out on purpose: the canonical
// rating labels are routed by name, and any other label in 9 must fall to
// general.
var wd14Category = map[int]string{
	0: "general",
	1: "artist",
	3: "copyright",
	4: "character",
	5: "meta",
}

type categoryResolution struct {
	catID    int64
	catName  string
	skip     bool
	override bool // true when a dispatch rule produced this result
}

func resolveCategory(profile Profile, label tagLabel, catIDs map[string]int64, dispatch *DispatchTable) categoryResolution {
	if rule, ok := dispatch.Lookup(label.name); ok {
		if rule.Drop {
			return categoryResolution{skip: true, override: true}
		}
		return categoryResolution{
			catID:    rule.CatID,
			catName:  rule.CatName,
			override: true,
		}
	}
	if tags.IsCanonicalRating(label.name) {
		return categoryResolution{
			catID:   catIDs["rating"],
			catName: "rating",
		}
	}
	name := "general"
	switch {
	case profile.CategoryScheme == "single_general":
	case label.categoryName != "":
		name = label.categoryName
	case profile.CategoryScheme == "wd14_numeric":
		name = cmp.Or(wd14Category[label.categoryID], "general")
	}
	name = cmp.Or(profile.CategoryMap[name], name)
	if _, ok := catIDs[name]; !ok {
		name = "general"
	}
	return categoryResolution{catID: catIDs[name], catName: name}
}
