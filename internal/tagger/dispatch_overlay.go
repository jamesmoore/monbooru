package tagger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/monbooru/monbooru/internal/fsx"
)

func MergedDispatchRules(modelPath, taggerName string) []DispatchEntry {
	merged := map[string]DispatchEntry{}
	for _, e := range EmbeddedDispatchRules(taggerName) {
		merged[e.Source] = e
	}
	for _, e := range OverlayDispatchRules(modelPath, taggerName) {
		merged[e.Source] = e
	}
	out := make([]DispatchEntry, 0, len(merged))
	for _, e := range merged {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

func MarshalDispatchDoc(rules []DispatchEntry) ([]byte, error) {
	return json.MarshalIndent(dispatchDoc{Version: dispatchSchemaVersion, Rules: rules}, "", "  ")
}

// Atomic writes alone would still lose one of two edits made from the
// same snapshot.
var overlayMu sync.Mutex

func UpdateDispatchOverlay(modelPath, taggerName string, mutate func(map[string]DispatchEntry) error) error {
	overlayMu.Lock()
	defer overlayMu.Unlock()
	overlay := map[string]DispatchEntry{}
	for _, e := range OverlayDispatchRules(modelPath, taggerName) {
		overlay[e.Source] = e
	}
	if err := mutate(overlay); err != nil {
		return err
	}
	rules := make([]DispatchEntry, 0, len(overlay))
	for _, e := range overlay {
		rules = append(rules, e)
	}
	return SaveDispatchOverlay(modelPath, taggerName, rules)
}

// The overlay stays a pure delta against stock, and an empty one is
// deleted: the file existing is what "differs from stock" means.
func SaveDispatchOverlay(modelPath, taggerName string, rules []DispatchEntry) error {
	embedded := map[string]DispatchEntry{}
	for _, e := range EmbeddedDispatchRules(taggerName) {
		embedded[e.Source] = e
	}
	kept := make([]DispatchEntry, 0, len(rules))
	for _, r := range rules {
		if def, ok := embedded[r.Source]; ok && def == r {
			continue
		}
		kept = append(kept, r)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Source < kept[j].Source })

	path := filepath.Join(modelPath, taggerName, "dispatch.json")
	if len(kept) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(dispatchDoc{Version: dispatchSchemaVersion, Rules: kept}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsx.WriteAtomic(path, ".dispatch.json.*", func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}
