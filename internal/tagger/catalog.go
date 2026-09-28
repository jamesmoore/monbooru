package tagger

import (
	"cmp"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/monbooru/monbooru/internal/logx"
)

//go:embed catalog_default.json
var defaultCatalogJSON []byte

//go:embed dispatch_default/*.json
var defaultDispatchFS embed.FS

// Monbooru never fetches these URLs itself: Settings only renders the
// commands, so the app makes no outbound request.
type CatalogEntry struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Files       []CatalogFile `json:"files"`
	// Gated entries need a Hugging Face login; the snippets expect
	// HF_TOKEN in the shell.
	Gated             bool               `json:"gated,omitempty"`
	DefaultThreshold  float64            `json:"default_threshold,omitempty"`
	DefaultThresholds map[string]float64 `json:"default_thresholds,omitempty"`
	DefaultTopK       map[string]int     `json:"default_top_k,omitempty"`
}

type CatalogFile struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
}

type catalogDoc struct {
	Version int            `json:"version"`
	Models  []CatalogEntry `json:"models"`
}

func LoadCatalog(modelPath string) []CatalogEntry {
	var doc catalogDoc
	if err := json.Unmarshal(defaultCatalogJSON, &doc); err != nil {
		return nil
	}
	out := append([]CatalogEntry(nil), doc.Models...)

	if data, err := os.ReadFile(filepath.Join(modelPath, "models.json")); err == nil {
		var override catalogDoc
		if err := json.Unmarshal(data, &override); err == nil {
			byName := map[string]int{}
			for i, e := range out {
				byName[e.Name] = i
			}
			seen := map[string]bool{}
			for _, e := range override.Models {
				if i, ok := byName[e.Name]; ok {
					if seen[e.Name] {
						logx.Warnf("models.json: duplicate tagger name %q; keeping the last entry", e.Name)
					}
					out[i] = e
				} else {
					byName[e.Name] = len(out)
					out = append(out, e)
				}
				seen[e.Name] = true
			}
		}
	}
	return out
}

// $HF_TOKEN sits in double quotes so the shell running the snippet expands it.
func (c CatalogEntry) curlSteps(targetDir string) []string {
	auth := ""
	if c.Gated {
		auth = `-H "Authorization: Bearer $HF_TOKEN" `
	}
	steps := []string{"mkdir -p " + shellSingleQuote(targetDir)}
	for _, f := range c.Files {
		dst := targetDir + "/" + f.Filename
		steps = append(steps, "curl -L "+auth+"-o "+shellSingleQuote(dst)+" "+shellSingleQuote(f.URL))
	}
	return steps
}

// HostCommand's paths are relative to the model path.
func (c CatalogEntry) HostCommand() string { return strings.Join(c.curlSteps(c.Name), " && \\\n") }

// The host shell expands HF_TOKEN into -e; the single-quoted inner script
// leaves its own $HF_TOKEN to the container's shell.
func (c CatalogEntry) DockerCommand(containerName string) string {
	containerName = cmp.Or(containerName, "monbooru")
	env := ""
	if c.Gated {
		env = `-e HF_TOKEN="$HF_TOKEN" `
	}
	inner := strings.Join(c.curlSteps("/models/"+c.Name), " && ")
	return fmt.Sprintf("docker exec %s%s sh -c %s", env, containerName, shellSingleQuote(inner))
}

func shellSingleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
