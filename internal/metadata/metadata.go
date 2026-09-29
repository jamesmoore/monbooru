// Package metadata reads generation parameters, EXIF and ComicInfo out of
// files. Every input is untrusted: no length, offset or type is believed,
// and a malformed file is a miss, not a failure.
package metadata

import (
	"os"
	"sort"

	"github.com/monbooru/monbooru/internal/models"
)

func Extract(path, fileType string) (*models.SDMetadata, *models.ComfyUIMetadata, error) {
	switch fileType {
	case "png":
		sd, comfy, err := extractFromPNG(path)
		return sd, comfy, err
	case "jpeg":
		sd, err := extractSDFromJPEG(path)
		return sd, nil, err
	case "webp":
		return extractSDFromWebP(path), nil, nil
	default:
		return nil, nil, nil
	}
}

// ExtractGeneric returns the key-value pairs the SD and ComfyUI parsers
// leave unread.
func ExtractGeneric(path, fileType string) []models.SDParam {
	switch fileType {
	case "png":
		return genericFromPNG(path)
	case "jpeg":
		return genericFromEXIF(path)
	case "webp":
		return genericFromWebP(path)
	default:
		return nil
	}
}

func genericFromPNG(path string) []models.SDParam {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	chunks, err := readPNGTextChunks(f)
	if err != nil {
		return nil
	}
	skip := map[string]bool{"parameters": true, "prompt": true, "workflow": true}
	keys := make([]string, 0, len(chunks))
	for k := range chunks {
		if skip[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]models.SDParam, 0, len(keys))
	for _, k := range keys {
		out = append(out, models.SDParam{Key: k, Val: chunks[k]})
	}
	return out
}

func genericFromEXIF(path string) []models.SDParam {
	x := decodeJPEGEXIF(path)
	if x == nil {
		return nil
	}
	return collectEXIFTags(x)
}

func collectEXIFTags(x *exifData) []models.SDParam {
	out := make([]models.SDParam, 0, len(x.tags))
	x.walk(func(name string, tag *exifTag) {
		if name == userCommentField {
			return
		}
		out = append(out, models.SDParam{Key: name, Val: tag.String()})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func EXIFOrientation(path, fileType string) (int, bool) {
	var x *exifData
	switch fileType {
	case "jpeg":
		x = decodeJPEGEXIF(path)
	case "webp":
		x, _ = decodeWebPEXIF(path)
	}
	if x == nil {
		return 0, false
	}
	tag, ok := x.get("Orientation")
	if !ok {
		return 0, false
	}
	v, ok := tag.intAt(0)
	if !ok {
		return 0, false
	}
	return int(v), true
}
