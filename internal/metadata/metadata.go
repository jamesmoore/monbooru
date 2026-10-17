// Package metadata reads generation parameters, EXIF and ComicInfo out of
// files. Every input is untrusted: no length, offset or type is believed,
// and a malformed file is a miss, not a failure.
package metadata

import (
	"maps"
	"os"
	"slices"
	"sort"

	"github.com/monbooru/monbooru/internal/models"
)

func Extract(path, fileType string) (*models.SDMetadata, *models.ComfyUIMetadata) {
	switch fileType {
	case "png":
		return extractFromPNG(path)
	case "jpeg":
		return sdFromEXIF(decodeJPEGEXIF(path)), nil
	case "webp":
		return sdFromEXIF(decodeWebPEXIF(path)), nil
	default:
		return nil, nil
	}
}

// ExtractGeneric returns the key-value pairs the SD and ComfyUI parsers
// leave unread.
func ExtractGeneric(path, fileType string) []models.SDParam {
	switch fileType {
	case "png":
		return genericFromPNG(path)
	case "jpeg":
		return collectEXIFTags(decodeJPEGEXIF(path))
	case "webp":
		return collectEXIFTags(decodeWebPEXIF(path))
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
	for _, p := range pngTextParsers {
		delete(chunks, p.Key)
	}
	out := make([]models.SDParam, 0, len(chunks))
	for _, k := range slices.Sorted(maps.Keys(chunks)) {
		out = append(out, models.SDParam{Key: k, Val: chunks[k]})
	}
	return out
}

// A JPEG APP1 segment's ceiling; a WebP EXIF chunk can be far larger.
const maxNumericEXIFBytes = 64 * 1024

func collectEXIFTags(x *exifData) []models.SDParam {
	if x == nil {
		return nil
	}
	out := make([]models.SDParam, 0, len(x.tags))
	for name, tag := range x.tags {
		numeric := tag.typ != exifASCII && tag.typ != exifUndefined
		if name == userCommentField || numeric && len(tag.val) > maxNumericEXIFBytes {
			continue
		}
		out = append(out, models.SDParam{Key: name, Val: tag.String()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func EXIFOrientation(path, fileType string) (int, bool) {
	var x *exifData
	switch fileType {
	case "jpeg":
		x = decodeJPEGEXIF(path)
	case "webp":
		x = decodeWebPEXIF(path)
	}
	if x == nil {
		return 0, false
	}
	tag, ok := x.tags["Orientation"]
	if !ok {
		return 0, false
	}
	v, ok := tag.intAt(0)
	if !ok {
		return 0, false
	}
	return int(v), true
}
