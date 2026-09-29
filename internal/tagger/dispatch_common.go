package tagger

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/logx"
)

const dispatchSchemaVersion = 1

type dispatchDoc struct {
	Version int             `json:"version"`
	Rules   []DispatchEntry `json:"rules"`
}

// DispatchEntry routes Source into Category, renamed to Name when set; an
// empty Category drops the label.
type DispatchEntry struct {
	Source   string `json:"source"`
	Category string `json:"category"`
	Name     string `json:"name,omitempty"`
}

func DispatchTargetCategories(modelPath, taggerName string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(entries []DispatchEntry) {
		for _, e := range entries {
			if e.Category == "" || seen[e.Category] {
				continue
			}
			seen[e.Category] = true
			out = append(out, e.Category)
		}
	}
	add(EmbeddedDispatchRules(taggerName))
	add(OverlayDispatchRules(modelPath, taggerName))
	return out
}

func OverlayRuleCount(modelPath, taggerName string) int {
	return len(OverlayDispatchRules(modelPath, taggerName))
}

func EmbeddedDispatchRules(taggerName string) []DispatchEntry {
	data, err := defaultDispatchFS.ReadFile("dispatch_default/" + taggerName + ".json")
	if err != nil {
		return nil
	}
	return parseDispatchDoc(data, "embedded "+taggerName)
}

func OverlayDispatchRules(modelPath, taggerName string) []DispatchEntry {
	p := filepath.Join(modelPath, taggerName, "dispatch.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	return parseDispatchDoc(data, p)
}

func parseDispatchDoc(data []byte, source string) []DispatchEntry {
	var doc dispatchDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		logx.Warnf("tagger: %s dispatch parse failed: %v", source, err)
		return nil
	}
	if doc.Version != dispatchSchemaVersion {
		logx.Warnf("tagger: %s dispatch schema version %d unsupported (want %d)", source, doc.Version, dispatchSchemaVersion)
		return nil
	}
	return doc.Rules
}
