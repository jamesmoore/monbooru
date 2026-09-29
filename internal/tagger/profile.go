package tagger

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	switch p.Layout {
	case "nhwc", "nchw":
	default:
		return fmt.Errorf("profile %q: bad layout %q", p.Name, p.Layout)
	}
	switch p.Channels {
	case "rgb", "bgr":
	default:
		return fmt.Errorf("profile %q: bad channels %q", p.Name, p.Channels)
	}
	switch p.Normalize {
	case "none", "div255", "imagenet", "clip":
	default:
		return fmt.Errorf("profile %q: bad normalize %q", p.Name, p.Normalize)
	}
	switch p.Pad {
	case "white_square", "mean_color_aspect":
	default:
		return fmt.Errorf("profile %q: bad pad %q", p.Name, p.Pad)
	}
	switch p.Activation {
	case "sigmoid_in_model", "logits":
	default:
		return fmt.Errorf("profile %q: bad activation %q", p.Name, p.Activation)
	}
	switch p.LabelFormat {
	case "wd14_csv", "joytag_txt", "camie_json":
	default:
		return fmt.Errorf("profile %q: bad label_format %q", p.Name, p.LabelFormat)
	}
	switch p.CategoryScheme {
	case "wd14_numeric", "single_general", "name_string":
	default:
		return fmt.Errorf("profile %q: bad category_scheme %q", p.Name, p.CategoryScheme)
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

func mergeProfile(dst *Profile, src Profile) {
	if src.InputSize != 0 {
		dst.InputSize = src.InputSize
	}
	if src.Layout != "" {
		dst.Layout = src.Layout
	}
	if src.Channels != "" {
		dst.Channels = src.Channels
	}
	if src.Normalize != "" {
		dst.Normalize = src.Normalize
	}
	if src.Pad != "" {
		dst.Pad = src.Pad
	}
	if src.FillColor != ([3]uint8{}) {
		dst.FillColor = src.FillColor
	}
	if src.Activation != "" {
		dst.Activation = src.Activation
	}
	if src.LabelFormat != "" {
		dst.LabelFormat = src.LabelFormat
	}
	if src.CategoryScheme != "" {
		dst.CategoryScheme = src.CategoryScheme
	}
	if src.OutputIndex != 0 {
		dst.OutputIndex = src.OutputIndex
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

// SidecarProfileFields must test the same fields mergeProfile does.
func SidecarProfileFields(modelPath, taggerName string) []string {
	sidecar, ok := parseSidecarProfile(modelPath, taggerName)
	if !ok {
		return nil
	}
	var out []string
	if sidecar.InputSize != 0 {
		out = append(out, "input_size")
	}
	if sidecar.Layout != "" {
		out = append(out, "layout")
	}
	if sidecar.Channels != "" {
		out = append(out, "channels")
	}
	if sidecar.Normalize != "" {
		out = append(out, "normalize")
	}
	if sidecar.Pad != "" {
		out = append(out, "pad")
	}
	if sidecar.FillColor != ([3]uint8{}) {
		out = append(out, "fill_color")
	}
	if sidecar.Activation != "" {
		out = append(out, "activation")
	}
	if sidecar.LabelFormat != "" {
		out = append(out, "label_format")
	}
	if sidecar.CategoryScheme != "" {
		out = append(out, "category_scheme")
	}
	if sidecar.OutputIndex != 0 {
		out = append(out, "output_index")
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
