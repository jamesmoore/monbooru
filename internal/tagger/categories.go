package tagger

import "cmp"

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

// Only the four canonical names: storeResults drops any other name routed
// to rating.
var wd14RatingTags = map[string]bool{
	"general": true, "sensitive": true, "questionable": true, "explicit": true,
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
	if wd14RatingTags[label.name] {
		return categoryResolution{
			catID:   catIDs["rating"],
			catName: "rating",
		}
	}
	switch profile.CategoryScheme {
	case "wd14_numeric":
		name := wd14Category[label.categoryID]
		name = cmp.Or(name, "general")
		return categoryResolution{catID: catIDs[name], catName: name}
	case "single_general":
		return categoryResolution{catID: catIDs["general"], catName: "general"}
	case "name_string":
		name := label.categoryName
		if _, ok := catIDs[name]; !ok {
			name = "general"
		}
		return categoryResolution{catID: catIDs[name], catName: name}
	}
	return categoryResolution{catID: catIDs["general"], catName: "general"}
}
