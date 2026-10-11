package tagger

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/logx"
)

const profileSchemaVersion = 1

// Profile fields left zero defer to the previous resolution layer, so a
// sidecar can override a single axis.
type Profile struct {
	Name string `json:"-"`

	// Zero reads the size from the model's input.
	InputSize      int      `json:"input_size,omitempty"`
	Layout         string   `json:"layout,omitempty"`
	Channels       string   `json:"channels,omitempty"`
	Normalize      string   `json:"normalize,omitempty"`
	Pad            string   `json:"pad,omitempty"`
	FillColor      [3]uint8 `json:"fill_color,omitempty"`
	Activation     string   `json:"activation,omitempty"`
	LabelFormat    string   `json:"label_format,omitempty"`
	CategoryScheme string   `json:"category_scheme,omitempty"`
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
		{"normalize", p.Normalize, []string{"none", "div255", "imagenet", "clip"}},
		{"pad", p.Pad, []string{"white_square", "mean_color_aspect"}},
		{"activation", p.Activation, []string{"sigmoid_in_model", "logits"}},
		{"label_format", p.LabelFormat, []string{"wd14_csv", "joytag_txt", "camie_json"}},
		{"category_scheme", p.CategoryScheme, []string{"wd14_numeric", "single_general", "name_string"}},
	} {
		if !slices.Contains(axis.allowed, axis.value) {
			return fmt.Errorf("profile %q: bad %s %q", p.Name, axis.key, axis.value)
		}
	}
	return nil
}

//go:embed profile_default
var defaultProfileFS embed.FS

var wd14Profile = Profile{
	Layout:         "nhwc",
	Channels:       "bgr",
	Normalize:      "none",
	Pad:            "white_square",
	Activation:     "sigmoid_in_model",
	LabelFormat:    "wd14_csv",
	CategoryScheme: "wd14_numeric",
}

var joytagProfile = Profile{
	Layout:         "nchw",
	Channels:       "rgb",
	Normalize:      "clip",
	Pad:            "white_square",
	Activation:     "logits",
	LabelFormat:    "joytag_txt",
	CategoryScheme: "single_general",
}

func ResolveProfile(modelPath, taggerName, tagsFile string) (Profile, error) {
	merged := Profile{Name: taggerName}
	if heur := heuristicProfile(tagsFile); heur != nil {
		mergeProfile(&merged, *heur)
	}
	if embedded, ok := parseEmbeddedProfile(taggerName); ok {
		mergeProfile(&merged, embedded)
	}
	if sidecar, ok := parseSidecarProfile(modelPath, taggerName); ok {
		mergeProfile(&merged, sidecar)
	}
	if err := merged.validate(); err != nil {
		return Profile{}, err
	}
	return merged, nil
}

func heuristicProfile(tagsFile string) *Profile {
	switch strings.ToLower(filepath.Ext(tagsFile)) {
	case ".csv":
		p := wd14Profile
		return &p
	case ".txt":
		p := joytagProfile
		return &p
	}
	return nil
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
	{"pad", func(d *Profile, s Profile) bool { return setIf(&d.Pad, s.Pad) }},
	{"fill_color", func(d *Profile, s Profile) bool { return setIf(&d.FillColor, s.FillColor) }},
	{"activation", func(d *Profile, s Profile) bool { return setIf(&d.Activation, s.Activation) }},
	{"label_format", func(d *Profile, s Profile) bool { return setIf(&d.LabelFormat, s.LabelFormat) }},
	{"category_scheme", func(d *Profile, s Profile) bool { return setIf(&d.CategoryScheme, s.CategoryScheme) }},
	{"output_index", func(d *Profile, s Profile) bool { return setIf(&d.OutputIndex, s.OutputIndex) }},
}

func mergeProfile(dst *Profile, src Profile) {
	for _, axis := range profileAxes {
		axis.merge(dst, src)
	}
}

func parseEmbeddedProfile(taggerName string) (Profile, bool) {
	data, err := defaultProfileFS.ReadFile("profile_default/" + taggerName + ".json")
	if err != nil {
		return Profile{}, false
	}
	return parseProfileDoc(data, "embedded "+taggerName)
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
func (p Profile) EmittedCategories() []string {
	switch p.CategoryScheme {
	case "wd14_numeric":
		return []string{"general", "artist", "character", "copyright", "meta", "rating"}
	case "single_general":
		return []string{"general"}
	case "name_string":
		// The seven categories Camie's metadata declares.
		return []string{"artist", "character", "copyright", "general", "meta", "rating", "year"}
	}
	return nil
}
