// Package searchkw is the search filter-keyword vocabulary; it imports
// nothing of ours.
package searchkw

import "strings"

// Keywords is in the order the system: cheat sheet lists them.
var Keywords = []string{
	"fav",
	"inbox",
	"ai",
	"source",
	"cat",
	"width",
	"height",
	"date",
	"missing",
	"tagged",
	"autotagged",
	"stale",
	"folder",
	"folderonly",
	"generated",
	"rating",
	"type",
	"collection",
	"pages",
	"name",
	"size",
	"mime",
	"ratio",
	"tagcount",
	"duration",
	"hash",
	"md5",
	"prompt",
	"comfyui",
	"model",
	"sampler",
	"seed",
	"via",
	"phash",
	"relation",
	"similar",
	"id",
	"batch",
	"lookup",
	"upgrade",
}

// system is no filter, but a system: query must not be probed as a
// category-qualified tag.
var keywordSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(Keywords)+1)
	m["system"] = struct{}{}
	for _, k := range Keywords {
		m[k] = struct{}{}
	}
	return m
}()

func IsKeyword(s string) bool {
	_, ok := keywordSet[s]
	return ok
}

// For a key outside rangeKeys, its row is also the full set of accepted
// values. cat: has no row because its values come from tag_categories.
var Expansions = map[string][]string{
	"fav":        {"true", "false"},
	"inbox":      {"true", "false"},
	"ai":         {"a1111", "comfyui", "none", "any", "sd"},
	"source":     {"none", "any"},
	"width":      {">=", "<=", ">", "<", "=", ".."},
	"height":     {">=", "<=", ">", "<", "=", ".."},
	"date":       {">", "<", ">=", "<=", "=", ".."},
	"missing":    {"true", "false"},
	"tagged":     {"true", "false", "user"},
	"autotagged": {"true", "false"},
	"stale":      {"any", "none"},
	"rating":     {"general", "sensitive", "questionable", "explicit"},
	"type":       {"image", "archive", "animated"},
	"pages":      {">=", "<=", ">", "<", "=", ".."},
	"size":       {">=", "<=", ">", "<", "=", ".."},
	"ratio":      {">=", "<=", ">", "<", "=", ".."},
	"tagcount":   {">=", "<=", ">", "<", "=", ".."},
	"duration":   {">=", "<=", ">", "<", "=", ".."},
	"mime":       {"jpeg", "png", "webp", "avif", "jxl", "gif", "mp4", "webm", "cbz"},
	"via":        {"ingest", "upload", "extract", "generate"},
	"relation":   {"duplicate", "original", "alternate", "version", "derivative", "source", "collection", "any", "none"},
	"lookup":     {"never", "due", "missed", "exhausted", "off"},
	"upgrade":    {"any", "bigger", "none", "unknown", "sample", "kept"},
}

// rangeKeys have Expansions rows that are only hints: operators, or
// shortcuts beside an open tag, site, source or origin label.
var rangeKeys = map[string]bool{
	"width": true, "height": true, "date": true, "size": true,
	"ratio": true, "tagcount": true, "duration": true, "pages": true,
	"stale": true, "source": true, "upgrade": true,
	"tagged": true, "autotagged": true, "via": true,
}

var closedVocab = func() map[string]map[string]struct{} {
	m := make(map[string]map[string]struct{}, len(Expansions))
	for key, vals := range Expansions {
		if rangeKeys[key] {
			continue
		}
		set := make(map[string]struct{}, len(vals))
		for _, v := range vals {
			set[v] = struct{}{}
		}
		m[key] = set
	}
	return m
}()

func ValueKnown(key, val string) bool {
	set, ok := closedVocab[key]
	if !ok {
		return true
	}
	for _, part := range strings.Split(strings.ToLower(val), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := set[part]; !ok {
			return false
		}
	}
	return true
}

var Descriptions = map[string]string{
	"fav":        "favorite images",
	"inbox":      "in inbox",
	"ai":         "AI generation tool",
	"source":     "external source label",
	"cat":        "by tag category",
	"width":      "image width",
	"height":     "image height",
	"date":       "ingestion date",
	"missing":    "files gone from disk",
	"tagged":     "has any tag, or by source",
	"autotagged": "has auto-tag, or by tagger",
	"stale":      "tags a source dropped",
	"folder":     "folder (recursive)",
	"folderonly": "folder (exact)",
	"generated":  "generation recipe",
	"rating":     "safety rating",
	"type":       "image / archive / animated",
	"collection": "collection label",
	"pages":      "page count",
	"name":       "file name",
	"size":       "file size",
	"mime":       "file type",
	"ratio":      "aspect ratio (width / height)",
	"tagcount":   "number of tags",
	"duration":   "video duration in seconds",
	"hash":       "sha256 or md5 digest",
	"md5":        "md5 digest",
	"prompt":     "SD / ComfyUI prompt",
	"comfyui":    "ComfyUI workflow node or input",
	"model":      "SD / ComfyUI model",
	"sampler":    "SD / ComfyUI sampler",
	"seed":       "SD / ComfyUI seed",
	"via":        "added via",
	"phash":      "perceptual hash (16-hex; ~d for Hamming distance)",
	"relation":   "declared relation",
	"similar":    "tag similarity to image id (~score 0..1 for a threshold)",
	"id":         "image id",
	"batch":      "upload batch id",
	"lookup":     "scheduled lookup state",
	"upgrade":    "source serves a different file",
}

// Shared by four keys, so nothing may write to it.
var numericComparisons = map[string]string{
	">=": "at least",
	"<=": "at most",
	">":  "more than",
	"<":  "less than",
	"=":  "exactly",
	"..": "range",
}

var ExpansionDescriptions = map[string]map[string]string{
	"date": {
		">":  "after",
		"<":  "before",
		">=": "on or after",
		"<=": "on or before",
		"=":  "exactly",
		"..": "range",
	},
	"width":  numericComparisons,
	"height": numericComparisons,
	"ai": {
		"a1111":   "A1111 / Forge",
		"comfyui": "ComfyUI",
		"none":    "no metadata",
		"any":     "any AI tool",
		"sd":      "alias of a1111",
	},
	"pages": numericComparisons,
	"type": {
		"image":    "regular images (jpeg / png / webp / avif / jxl)",
		"archive":  "cbz / zip archives",
		"animated": "gif / mp4 / webm",
	},
	"size": {
		">=": "at least (bytes; suffix KB/MB/GB)",
		"<=": "at most",
		">":  "more than",
		"<":  "less than",
		"=":  "exactly",
		"..": "range",
	},
	"ratio": {
		">=": "wider than (e.g. 1.5 = 3:2)",
		"<=": "taller than",
		">":  "wider than",
		"<":  "taller than",
		"=":  "exact ratio",
		"..": "range",
	},
	"tagcount": numericComparisons,
	"duration": {
		">=": "at least N seconds",
		"<=": "at most N seconds",
		">":  "longer than",
		"<":  "shorter than",
		"=":  "exactly N seconds",
		"..": "range",
	},
	"via": {
		"ingest":   "watcher or sync",
		"upload":   "web upload form",
		"extract":  "page extracted in the reader",
		"generate": "archive built from a collection",
	},
	"source": {
		"none": "no source at all",
		"any":  "any source",
	},
	"tagged": {
		"user": "added by hand",
	},
	"lookup": {
		"never":     "not tried yet",
		"due":       "queued for next scheduled lookup",
		"missed":    "waiting out a backoff",
		"exhausted": "nothing found",
		"off":       "never look this up",
	},
}
