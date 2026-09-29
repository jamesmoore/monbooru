package tagger

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
)

const (
	DefaultModelFile           = "model.onnx"
	DefaultTagsFile            = "tags.csv"
	DefaultTextTagsFile        = "tags.txt"
	DefaultConfidenceThreshold = 0.4
)

// The name becomes a TOML key and a folder under the model path, which the
// settings delete hands to os.RemoveAll.
var taggerNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func ValidateTaggerName(name string) error {
	if name == "" {
		return fmt.Errorf("tagger name must not be empty")
	}
	if !taggerNameRe.MatchString(name) {
		return fmt.Errorf("tagger name %q must match [A-Za-z0-9_-]+", name)
	}
	return nil
}

type TaggerStatus struct {
	config.TaggerInstance
	Available bool
	Reason    string
}

// DiscoverTaggers keeps a row for a configured tagger whose folder is gone.
func DiscoverTaggers(cfg *config.Config) []TaggerStatus {
	byName := map[string]config.TaggerInstance{}
	order := []string{}

	catalogDefaults := catalogDefaultsByName(cfg.Paths.ModelPath)

	if entries, err := os.ReadDir(cfg.Paths.ModelPath); err == nil {
		for _, e := range entries {
			name := e.Name()
			// Stat, not the entry's type: a symlinked model directory
			// reports as a link.
			fi, err := os.Stat(filepath.Join(cfg.Paths.ModelPath, name))
			if err != nil || !fi.IsDir() {
				continue
			}
			if !hasTaggerFiles(filepath.Join(cfg.Paths.ModelPath, name)) {
				continue
			}
			byName[name] = SeedTaggerInstance(name, true, catalogDefaults[name])
			order = append(order, name)
		}
	}

	for _, t := range cfg.Tagger.Taggers {
		if _, seen := byName[t.Name]; !seen {
			order = append(order, t.Name)
		}
		byName[t.Name] = t
	}

	noLibrary := ""
	if buildSupportsInference() {
		noLibrary = missingRuntimeLibrary()
	}

	out := make([]TaggerStatus, 0, len(order))
	for _, name := range order {
		t := byName[name]
		dir := filepath.Join(cfg.Paths.ModelPath, name)
		t.ModelFile, t.TagsFile = resolveTaggerFiles(dir, t.ModelFile, t.TagsFile)

		status := TaggerStatus{TaggerInstance: t, Available: true}
		onnxPath := filepath.Join(dir, t.ModelFile)
		tagsPath := filepath.Join(dir, t.TagsFile)
		switch {
		case noLibrary != "":
			status.Available = false
			status.Reason = noLibrary
		case statMissing(onnxPath):
			status.Available = false
			status.Reason = "missing " + t.ModelFile
		case statMissing(tagsPath):
			status.Available = false
			status.Reason = "missing " + t.TagsFile
		}
		out = append(out, status)
	}
	return out
}

func statMissing(path string) bool {
	_, err := os.Stat(path)
	return err != nil
}

// EnabledTaggers ignores gallery scoping; a job on one gallery uses
// EnabledTaggersForGallery.
func EnabledTaggers(cfg *config.Config) []TaggerStatus { return enabledTaggers(cfg, nil) }

func SelectForGallery(cfg *config.Config, gallery, name string) ([]TaggerStatus, error) {
	enabled := EnabledTaggersForGallery(cfg, gallery)
	if name == "" {
		return enabled, nil
	}
	for _, t := range enabled {
		if t.Name == name {
			return []TaggerStatus{t}, nil
		}
	}
	return nil, fmt.Errorf("tagger %q is not enabled or available for gallery %q", name, gallery)
}

func EnabledTaggersForGallery(cfg *config.Config, gallery string) []TaggerStatus {
	return enabledTaggers(cfg, func(t TaggerStatus) bool { return t.AppliesToGallery(gallery) })
}

// Present separates "no tagger set up", which hides the auto-tag
// controls, from "set up but unusable", which only disables them.
func Present(cfg *config.Config) bool {
	return buildSupportsInference() && len(DiscoverTaggers(cfg)) > 0
}

// The unscoped listing passes no predicate: AppliesToGallery("") is false
// for a gallery-scoped tagger.
func enabledTaggers(cfg *config.Config, extra func(TaggerStatus) bool) []TaggerStatus {
	if !buildSupportsInference() {
		return nil
	}
	var out []TaggerStatus
	for _, t := range DiscoverTaggers(cfg) {
		if !t.Enabled || !t.Available {
			continue
		}
		if extra != nil && !extra(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// resolveTaggerFiles falls back to the default names so a missing file
// still gets named in the reason.
func resolveTaggerFiles(dir, explicitModel, explicitTags string) (string, string) {
	modelFile := explicitModel
	tagsFile := explicitTags

	var onnxFiles, labelFiles []string
	hasTagsCSV, hasTagsTXT := false, false
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			switch strings.ToLower(filepath.Ext(n)) {
			case ".onnx":
				onnxFiles = append(onnxFiles, n)
			case ".csv":
				labelFiles = append(labelFiles, n)
				if n == DefaultTagsFile {
					hasTagsCSV = true
				}
			case ".txt":
				labelFiles = append(labelFiles, n)
				if n == DefaultTextTagsFile {
					hasTagsTXT = true
				}
			case ".json":
				if !isTaggerSidecar(n) {
					labelFiles = append(labelFiles, n)
				}
			}
		}
	}

	if modelFile == "" {
		switch {
		case slices.Contains(onnxFiles, DefaultModelFile):
			modelFile = DefaultModelFile
		case len(onnxFiles) == 1:
			modelFile = onnxFiles[0]
		default:
			modelFile = DefaultModelFile
		}
	}

	if tagsFile == "" {
		switch {
		case hasTagsCSV:
			tagsFile = DefaultTagsFile
		case hasTagsTXT:
			tagsFile = DefaultTextTagsFile
		case len(labelFiles) == 1:
			tagsFile = labelFiles[0]
		default:
			tagsFile = DefaultTagsFile
		}
	}

	return modelFile, tagsFile
}

func isTaggerSidecar(name string) bool { return name == "tagger.json" || name == "dispatch.json" }

func hasTaggerFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		switch strings.ToLower(filepath.Ext(n)) {
		case ".onnx", ".csv", ".txt":
			return true
		case ".json":
			if !isTaggerSidecar(n) {
				return true
			}
		}
	}
	return false
}

func SeedTaggerInstance(name string, enabled bool, catalog *CatalogEntry) config.TaggerInstance {
	t := config.TaggerInstance{
		Name:                name,
		Enabled:             enabled,
		ConfidenceThreshold: DefaultConfidenceThreshold,
	}
	if catalog == nil {
		return t
	}
	if catalog.DefaultThreshold > 0 {
		t.ConfidenceThreshold = catalog.DefaultThreshold
	}
	if len(catalog.DefaultThresholds) > 0 {
		t.CategoryThresholds = make(map[string]float64, len(catalog.DefaultThresholds))
		for k, v := range catalog.DefaultThresholds {
			t.CategoryThresholds[k] = v
		}
	}
	if len(catalog.DefaultTopK) > 0 {
		t.PerCategoryTopK = make(map[string]int, len(catalog.DefaultTopK))
		for k, v := range catalog.DefaultTopK {
			t.PerCategoryTopK[k] = v
		}
	}
	return t
}

func catalogDefaultsByName(modelPath string) map[string]*CatalogEntry {
	cat := LoadCatalog(modelPath)
	out := make(map[string]*CatalogEntry, len(cat))
	for i := range cat {
		out[cat[i].Name] = &cat[i]
	}
	return out
}
