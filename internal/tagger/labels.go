package tagger

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	case "wd14_csv":
		return loadLabelsCSV(f)
	case "joytag_txt":
		return loadLabelsText(f)
	case "camie_json":
		return loadLabelsCamieJSON(f)
	}
	return nil, fmt.Errorf("loadLabels: unsupported label_format %q", profile.LabelFormat)
}

func loadLabelsCSV(f io.Reader) ([]tagLabel, error) {
	r := csv.NewReader(f)
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
		if len(rec) < 3 {
			continue
		}
		catID, _ := strconv.Atoi(strings.TrimSpace(rec[2]))
		name, ok := sanitizeLabel(rec[1], len(labels))
		labels = append(labels, tagLabel{
			name:        name,
			categoryID:  catID,
			placeholder: !ok,
		})
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
		labels = append(labels, tagLabel{
			name:        name,
			categoryID:  0,
			placeholder: !ok,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return labels, nil
}

// camieMetadata is the part of camie-tagger-v2-metadata.json the tagger reads.
type camieMetadata struct {
	DatasetInfo struct {
		TagMapping struct {
			IdxToTag      map[string]string `json:"idx_to_tag"`
			TagToCategory map[string]string `json:"tag_to_category"`
		} `json:"tag_mapping"`
	} `json:"dataset_info"`
}

func loadLabelsCamieJSON(f io.Reader) ([]tagLabel, error) {
	var doc camieMetadata
	if err := json.NewDecoder(f).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode camie metadata: %w", err)
	}
	idxMap := doc.DatasetInfo.TagMapping.IdxToTag
	if len(idxMap) == 0 {
		return nil, fmt.Errorf("camie metadata: dataset_info.tag_mapping.idx_to_tag is empty")
	}
	maxIdx := -1
	for k := range idxMap {
		i, err := strconv.Atoi(k)
		if err != nil || i < 0 {
			return nil, fmt.Errorf("camie metadata: bad idx %q", k)
		}
		if i > maxIdx {
			maxIdx = i
		}
	}
	labels := make([]tagLabel, maxIdx+1)
	cats := doc.DatasetInfo.TagMapping.TagToCategory
	for k, raw := range idxMap {
		i, _ := strconv.Atoi(k)
		name, ok := sanitizeLabel(raw, i)
		labels[i] = tagLabel{
			name:         name,
			categoryName: cats[raw],
			placeholder:  !ok,
		}
	}
	for i := range labels {
		if labels[i].name == "" {
			labels[i] = tagLabel{
				name:        fmt.Sprintf("_unsupported_%d", i),
				placeholder: true,
			}
		}
	}
	return labels, nil
}
