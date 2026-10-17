package tagger

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/tags"
)

func sanitizeLabel(raw string, idx int) (string, bool) {
	name := tags.NormalizeName(raw)
	if rs := []rune(name); len(rs) > 200 {
		name = string(rs[:200])
	}
	if name == "" || !tags.HasTagContent(name) {
		return fmt.Sprintf("_unsupported_%d", idx), false
	}
	return name, true
}

// tagLabel slices are indexed by the model's output channel: an unusable
// label becomes a placeholder, which inference must skip, rather than
// being dropped.
type tagLabel struct {
	name         string
	categoryID   int
	categoryName string
	placeholder  bool
}

func loadLabels(path string, profile Profile) ([]tagLabel, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	switch profile.LabelFormat {
	case "csv":
		return loadLabelsCSV(f)
	case "txt":
		return loadLabelsText(f)
	case "json":
		if profile.LabelJSON == nil {
			return nil, fmt.Errorf("loadLabels: label_format json needs label_json")
		}
		return loadLabelsJSON(f, *profile.LabelJSON)
	}
	return nil, fmt.Errorf("loadLabels: unsupported label_format %q", profile.LabelFormat)
}

// The third column is a WD14 category id when it parses as a number and
// a category name otherwise.
func loadLabelsCSV(f io.Reader) ([]tagLabel, error) {
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	if _, err := r.Read(); err != nil {
		return nil, err
	}
	var labels []tagLabel
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) < 2 {
			continue
		}
		name, ok := sanitizeLabel(rec[1], len(labels))
		l := tagLabel{name: name, placeholder: !ok}
		if len(rec) > 2 {
			cat := strings.TrimSpace(rec[2])
			if id, err := strconv.Atoi(cat); err == nil {
				l.categoryID = id
			} else {
				l.categoryName = cat
			}
		}
		labels = append(labels, l)
	}
	return labels, nil
}

func loadLabelsText(f io.Reader) ([]tagLabel, error) {
	var labels []tagLabel
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		raw := scanner.Text()
		if strings.TrimSpace(raw) == "" {
			continue
		}
		name, ok := sanitizeLabel(raw, len(labels))
		labels = append(labels, tagLabel{name: name, placeholder: !ok})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return labels, nil
}

// LabelJSON locates the labels in a JSON label file. Path and
// CategoryPath are dotted object keys from the root.
//
//	index_map: {"0": "tag", ...}, the key is the output index
//	list:      ["tag", ...], the position is the output index
//	blocks:    [{"name": category, "offset": first index, "tags": [...]}, ...];
//	           without offsets the blocks follow one another
type LabelJSON struct {
	Shape        string `json:"shape"`
	Path         string `json:"path,omitempty"`
	CategoryPath string `json:"category_path,omitempty"`
}

// Far past any model's output width; a stray index must not size the slice.
const maxLabels = 1 << 20

var camieLabelJSON = LabelJSON{
	Shape:        "index_map",
	Path:         "dataset_info.tag_mapping.idx_to_tag",
	CategoryPath: "dataset_info.tag_mapping.tag_to_category",
}

func jsonAt(doc any, path string) (any, error) {
	if path == "" {
		return doc, nil
	}
	for key := range strings.SplitSeq(path, ".") {
		obj, ok := doc.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("label json: %q is not an object at %q", path, key)
		}
		if doc, ok = obj[key]; !ok {
			return nil, fmt.Errorf("label json: no %q in %q", key, path)
		}
	}
	return doc, nil
}

func loadLabelsJSON(f io.Reader, spec LabelJSON) ([]tagLabel, error) {
	var doc any
	if err := json.NewDecoder(f).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode label json: %w", err)
	}
	node, err := jsonAt(doc, spec.Path)
	if err != nil {
		return nil, err
	}
	var cats map[string]any
	if spec.CategoryPath != "" {
		c, err := jsonAt(doc, spec.CategoryPath)
		if err != nil {
			return nil, err
		}
		if cats, _ = c.(map[string]any); cats == nil {
			return nil, fmt.Errorf("label json: %q is not an object", spec.CategoryPath)
		}
	}

	raw := map[int]string{}
	rawCat := map[int]string{}
	switch spec.Shape {
	case "index_map":
		m, ok := node.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("label json: index_map at %q is not an object", spec.Path)
		}
		for k, v := range m {
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 {
				return nil, fmt.Errorf("label json: bad index %q", k)
			}
			raw[i], _ = v.(string)
		}
	case "list":
		list, ok := node.([]any)
		if !ok {
			return nil, fmt.Errorf("label json: list at %q is not an array", spec.Path)
		}
		for i, v := range list {
			raw[i], _ = v.(string)
		}
	case "blocks":
		blocks, ok := node.([]any)
		if !ok {
			return nil, fmt.Errorf("label json: blocks at %q is not an array", spec.Path)
		}
		next := 0
		for _, b := range blocks {
			block, _ := b.(map[string]any)
			cat, _ := block["name"].(string)
			tagList, _ := block["tags"].([]any)
			if off, ok := block["offset"].(float64); ok {
				if off < 0 || off >= maxLabels {
					return nil, fmt.Errorf("label json: bad offset %v", off)
				}
				next = int(off)
			}
			for _, v := range tagList {
				raw[next], _ = v.(string)
				rawCat[next] = cat
				next++
			}
		}
	default:
		return nil, fmt.Errorf("label json: unknown shape %q", spec.Shape)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("label json: no labels at %q", spec.Path)
	}
	last := slices.Max(slices.Collect(maps.Keys(raw)))
	if last >= maxLabels {
		return nil, fmt.Errorf("label json: index %d past %d labels", last, maxLabels)
	}

	labels := make([]tagLabel, last+1)
	for i := range labels {
		r, ok := raw[i]
		if !ok || r == "" {
			labels[i] = tagLabel{name: fmt.Sprintf("_unsupported_%d", i), placeholder: true}
			continue
		}
		name, ok := sanitizeLabel(r, i)
		cat := rawCat[i]
		if cats != nil {
			cat, _ = cats[r].(string)
		}
		labels[i] = tagLabel{name: name, categoryName: cat, placeholder: !ok}
	}
	return labels, nil
}
