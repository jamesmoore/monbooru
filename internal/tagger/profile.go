package tagger

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

const profileSchemaVersion = 1

// Profile fields left zero defer to the previous resolution layer, so a
// sidecar can override a single axis.
type Profile struct {
	Name string `json:"-"`

	// Zero reads the size from the model's input.
	InputSize int    `json:"input_size,omitempty"`
	Layout    string `json:"layout,omitempty"`
	Channels  string `json:"channels,omitempty"`
	Normalize string `json:"normalize,omitempty"`
	// Read only under normalize "mean_std": (v/scale - mean) / std per
	// channel, scale 0 reading as 1.
	NormMean       *[3]float32 `json:"norm_mean,omitempty"`
	NormStd        *[3]float32 `json:"norm_std,omitempty"`
	NormScale      float32     `json:"norm_scale,omitempty"`
	Pad            string      `json:"pad,omitempty"`
	FillColor      *[3]uint8   `json:"fill_color,omitempty"`
	Activation     string      `json:"activation,omitempty"`
	LabelFormat    string      `json:"label_format,omitempty"`
	LabelJSON      *LabelJSON  `json:"label_json,omitempty"`
	CategoryScheme string      `json:"category_scheme,omitempty"`
	// Renames a label file's category before it meets tag_categories.
	CategoryMap map[string]string `json:"category_map,omitempty"`
	// Camie's second output, the refined head, is its production one.
	OutputIndex int `json:"output_index,omitempty"`
}

func (p Profile) fingerprint() string {
	data, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func (p Profile) validate() error {
	for _, axis := range []struct {
		key, value string
		allowed    []string
	}{
		{"layout", p.Layout, []string{"nhwc", "nchw"}},
		{"channels", p.Channels, []string{"rgb", "bgr"}},
		{"normalize", p.Normalize, []string{"none", "div255", "imagenet", "clip", "mean_std"}},
		{"pad", p.Pad, []string{"white_square", "mean_color_aspect", "fill"}},
		{"activation", p.Activation, []string{"sigmoid_in_model", "logits"}},
		{"label_format", p.LabelFormat, []string{"csv", "txt", "json"}},
		{"category_scheme", p.CategoryScheme, []string{"wd14_numeric", "single_general", "name_string"}},
	} {
		if !slices.Contains(axis.allowed, axis.value) {
			return fmt.Errorf("profile %q: bad %s %q", p.Name, axis.key, axis.value)
		}
	}
	custom := p.NormMean != nil || p.NormStd != nil || p.NormScale != 0
	switch {
	case p.Normalize == "mean_std" && (p.NormMean == nil || p.NormStd == nil):
		return fmt.Errorf("profile %q: normalize mean_std needs norm_mean and norm_std", p.Name)
	case p.Normalize != "mean_std" && custom:
		return fmt.Errorf("profile %q: norm_mean, norm_std and norm_scale need normalize mean_std, not %q", p.Name, p.Normalize)
	case p.NormStd != nil && slices.Contains(p.NormStd[:], 0):
		return fmt.Errorf("profile %q: norm_std has a zero", p.Name)
	case p.NormScale < 0:
		return fmt.Errorf("profile %q: norm_scale must be positive", p.Name)
	case p.Pad == "fill" && p.FillColor == nil:
		return fmt.Errorf("profile %q: pad fill needs fill_color", p.Name)
	case p.LabelFormat == "json" && p.LabelJSON == nil:
		return fmt.Errorf("profile %q: label_format json needs label_json", p.Name)
	case p.LabelJSON != nil && !slices.Contains([]string{"index_map", "list", "blocks"}, p.LabelJSON.Shape):
		return fmt.Errorf("profile %q: bad label_json shape %q", p.Name, p.LabelJSON.Shape)
	}
	return nil
}

// A label file's extension picks the shipped model whose profile a
// bare folder starts from.
var heuristicProfiles = map[string]string{".csv": "wd-swinv2", ".txt": "joytag"}

func ResolveProfile(modelPath, taggerName, tagsFile string) (Profile, error) {
	merged := Profile{Name: taggerName}
	if heur := heuristicProfile(tagsFile); heur != nil {
		mergeProfile(&merged, *heur)
	}
	if stock, ok := parseStockProfile(modelPath, taggerName); ok {
		mergeProfile(&merged, stock)
	}
	if sidecar, ok := parseSidecarProfile(modelPath, taggerName); ok {
		mergeProfile(&merged, sidecar)
	}
	merged.canonicalLabelFormat()
	if err := merged.validate(); err != nil {
		return Profile{}, err
	}
	return merged, nil
}

// The first label formats were named after the model that used them.
func (p *Profile) canonicalLabelFormat() {
	switch p.LabelFormat {
	case "wd14_csv":
		p.LabelFormat = "csv"
	case "joytag_txt":
		p.LabelFormat = "txt"
	case "camie_json":
		p.LabelFormat = "json"
		if p.LabelJSON == nil {
			spec := camieLabelJSON
			p.LabelJSON = &spec
		}
	}
}

func heuristicProfile(tagsFile string) *Profile {
	name, ok := heuristicProfiles[strings.ToLower(filepath.Ext(tagsFile))]
	if !ok {
		return nil
	}
	p, ok := parseStockProfile("", name)
	if !ok {
		return nil
	}
	return &p
}

func setIf[T comparable](dst *T, v T) bool {
	var zero T
	if v == zero {
		return false
	}
	*dst = v
	return true
}

var profileAxes = []struct {
	name  string
	merge func(dst *Profile, src Profile) bool
}{
	{"input_size", func(d *Profile, s Profile) bool { return setIf(&d.InputSize, s.InputSize) }},
	{"layout", func(d *Profile, s Profile) bool { return setIf(&d.Layout, s.Layout) }},
	{"channels", func(d *Profile, s Profile) bool { return setIf(&d.Channels, s.Channels) }},
	{"normalize", func(d *Profile, s Profile) bool { return setIf(&d.Normalize, s.Normalize) }},
	{"norm_mean", func(d *Profile, s Profile) bool { return setIf(&d.NormMean, s.NormMean) }},
	{"norm_std", func(d *Profile, s Profile) bool { return setIf(&d.NormStd, s.NormStd) }},
	{"norm_scale", func(d *Profile, s Profile) bool { return setIf(&d.NormScale, s.NormScale) }},
	{"pad", func(d *Profile, s Profile) bool { return setIf(&d.Pad, s.Pad) }},
	{"fill_color", func(d *Profile, s Profile) bool { return setIf(&d.FillColor, s.FillColor) }},
	{"activation", func(d *Profile, s Profile) bool { return setIf(&d.Activation, s.Activation) }},
	{"label_format", func(d *Profile, s Profile) bool { return setIf(&d.LabelFormat, s.LabelFormat) }},
	{"label_json", func(d *Profile, s Profile) bool { return setIf(&d.LabelJSON, s.LabelJSON) }},
	{"category_scheme", func(d *Profile, s Profile) bool { return setIf(&d.CategoryScheme, s.CategoryScheme) }},
	{"category_map", func(d *Profile, s Profile) bool {
		if s.CategoryMap == nil {
			return false
		}
		d.CategoryMap = s.CategoryMap
		return true
	}},
	{"output_index", func(d *Profile, s Profile) bool { return setIf(&d.OutputIndex, s.OutputIndex) }},
}

func mergeProfile(dst *Profile, src Profile) {
	for _, axis := range profileAxes {
		axis.merge(dst, src)
	}
}

func parseStockProfile(modelPath, taggerName string) (Profile, bool) {
	data, source, err := stockFile(modelPath, taggerName, stockProfile)
	if err != nil {
		return Profile{}, false
	}
	return parseProfileDoc(data, source)
}

func parseSidecarProfile(modelPath, taggerName string) (Profile, bool) {
	p := filepath.Join(modelPath, taggerName, "tagger.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return Profile{}, false
	}
	return parseProfileDoc(data, p)
}

type profileDoc struct {
	Version int     `json:"version"`
	Profile Profile `json:"profile"`
}

func parseProfileDoc(data []byte, source string) (Profile, bool) {
	var doc profileDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		logx.Warnf("tagger: %s profile parse failed: %v", source, err)
		return Profile{}, false
	}
	if doc.Version != profileSchemaVersion {
		logx.Warnf("tagger: %s profile schema version %d unsupported (want %d)", source, doc.Version, profileSchemaVersion)
		return Profile{}, false
	}
	return doc.Profile, true
}

func SidecarProfileFields(modelPath, taggerName string) []string {
	sidecar, ok := parseSidecarProfile(modelPath, taggerName)
	if !ok {
		return nil
	}
	var out []string
	for _, axis := range profileAxes {
		if axis.merge(&Profile{}, sidecar) {
			out = append(out, axis.name)
		}
	}
	return out
}

func ProfileExportDoc(p Profile) ([]byte, error) {
	return json.MarshalIndent(profileDoc{Version: profileSchemaVersion, Profile: p}, "", "  ")
}

// EmittedCategories is informational: a dispatch rule can still route a
// label into any category.
func EmittedCategories(modelPath, taggerName, tagsFile string) []string {
	profile, err := ResolveProfile(modelPath, taggerName, tagsFile)
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	mapped := func(c string) string { return cmp.Or(profile.CategoryMap[c], c) }
	switch profile.CategoryScheme {
	case "single_general":
		return []string{"general"}
	case "wd14_numeric":
		for _, c := range wd14Category {
			set[mapped(c)] = true
		}
		set["rating"] = true
	}
	labels, _ := loadLabels(filepath.Join(modelPath, taggerName, tagsFile), profile)
	for _, l := range labels {
		if l.placeholder {
			continue
		}
		if tags.IsCanonicalRating(l.name) {
			set["rating"] = true
		} else if l.categoryName != "" {
			set[mapped(l.categoryName)] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}
