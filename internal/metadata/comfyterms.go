package metadata

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// ComfyTermsVersion must be bumped whenever WorkflowTerms changes its
// output: the indexer re-derives every row stamped with an older one.
const ComfyTermsVersion = 1

// Keeps prompts out: nobody types one as an exact term, and prompt:
// already searches them.
const comfyTermMaxValue = 128

// So a workflow with a generated list in every node cannot flood the index.
const comfyTermsPerImage = 512

// WorkflowTerms is the sorted term set the comfyui: filter seeks:
// node=<class>, title=<title> and <input>=<value>.
func WorkflowTerms(raw string) []string {
	seen := make(map[string]struct{})
	for _, node := range comfyGraphNodes(raw) {
		for _, term := range comfyNodeTerms(node) {
			if len(seen) >= comfyTermsPerImage {
				break
			}
			seen[term] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(seen))
}

func comfyNodeTerms(node comfyGraphNode) []string {
	terms := []string{comfyTerm("node", node.ClassType)}
	if node.Title != "" && node.Title != node.ClassType {
		terms = append(terms, comfyTerm("title", node.Title))
	}
	for _, name := range slices.Sorted(maps.Keys(node.Inputs)) {
		if value, ok := comfyScalar(node.Inputs[name]); ok {
			terms = append(terms, comfyInputTerms(name, value)...)
		}
	}
	return slices.DeleteFunc(terms, func(t string) bool { return t == "" })
}

// Links only when the single term searches exactly what the row shows.
func comfyLinkTerm(name, value string) string {
	terms := comfyInputTerms(name, value)
	if len(terms) == 1 && terms[0] == comfyTerm(name, value) {
		return terms[0]
	}
	return ""
}

// A JSON string contributes its leaves instead, which keeps the LoRA
// stacks some custom nodes pack into one input searchable.
func comfyInputTerms(name, value string) []string {
	if name == "" || name == "seed" || name == "noise_seed" {
		return nil
	}
	if nested, ok := comfyNestedTerms(name, value); ok {
		return nested
	}
	if t := comfyTerm(name, value); t != "" {
		return []string{t}
	}
	return nil
}

// Array elements flatten by key, not index, so a term matches whichever
// slot a value sits in.
func comfyNestedTerms(name, value string) ([]string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || (value[0] != '[' && value[0] != '{') {
		return nil, false
	}
	var terms []string
	collect := func(obj map[string]json.RawMessage) {
		for _, key := range slices.Sorted(maps.Keys(obj)) {
			leaf, ok := comfyScalar(obj[key])
			if !ok {
				continue
			}
			if t := comfyTerm(name+"."+key, leaf); t != "" {
				terms = append(terms, t)
			}
		}
	}
	var arr []map[string]json.RawMessage
	if json.Unmarshal([]byte(value), &arr) == nil {
		for _, obj := range arr {
			collect(obj)
		}
		return terms, true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(value), &obj) == nil {
		collect(obj)
		return terms, true
	}
	return nil, false
}

func comfyTerm(name, value string) string {
	value = comfyTermText(value)
	if value == "" || len(value) > comfyTermMaxValue {
		return ""
	}
	return name + "=" + value
}

func comfyTermText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
