package galleryio

import (
	"archive/zip"
	"context"
	"maps"
	"slices"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/galleryio/compatibility"
	"github.com/monbooru/monbooru/internal/library"
)

func replaceFromCompatArchive(files []*zip.File, format, dbPath, thumbsPath string, b *gallery.Boundary, maxFileSizeMB int, quiesce func() error) (int, error) {
	result, err := compatibility.Translate(files, format)
	if err != nil {
		return 0, err
	}
	return ApplyLightReplace(
		toLightManifest(result.Manifest),
		translatedFilesFromCompat(result.Files),
		dbPath, thumbsPath, b, format, maxFileSizeMB, quiesce,
	)
}

func mergeFromCompatArchive(ctx context.Context, cx *library.Gallery, files []*zip.File, format string, maxFileSizeMB int, progress func(done, total int)) (MergeResult, error) {
	result, err := compatibility.Translate(files, format)
	if err != nil {
		return MergeResult{}, err
	}
	records := make([]mergeRecord, 0, len(result.Manifest.Images))
	for _, m := range result.Manifest.Images {
		records = append(records, mergeRecord{
			SHA256:     m.SHA256,
			Tags:       m.Tags,
			SourcePath: m.Path,
			zipEntry:   result.Files[m.Path],
		})
	}
	return applyMergeRecords(ctx, cx, records, format, maxFileSizeMB, progress)
}

func toLightManifest(m compatibility.Manifest) LightManifest {
	return LightManifest{Version: LightManifestVersion, Images: m.Images}
}

func translatedFilesFromCompat(in map[string]*zip.File) []translatedFile {
	rels := slices.Sorted(maps.Keys(in))
	out := make([]translatedFile, 0, len(rels))
	for _, r := range rels {
		out = append(out, translatedFile{rel: r, file: in[r]})
	}
	return out
}
