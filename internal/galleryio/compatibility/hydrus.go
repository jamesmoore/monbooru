package compatibility

import (
	"archive/zip"
	"bufio"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

func init() {
	Register(Provider{
		Name:      "hydrus",
		Detect:    detectHydrus,
		Translate: translateHydrus,
	})
}

func detectHydrus(entries []NormalizedEntry) bool {
	hasImage, hasSidecar := false, false
	for _, e := range entries {
		switch {
		case HasMediaExt(e.Rel):
			hasImage = true
		case strings.HasSuffix(strings.ToLower(e.Rel), ".txt"):
			hasSidecar = true
		}
		if hasImage && hasSidecar {
			return true
		}
	}
	return false
}

func translateHydrus(entries []NormalizedEntry) (Result, error) {
	images := map[string]*zip.File{}
	sidecars := map[string]*zip.File{}
	for _, e := range entries {
		if HasMediaExt(e.Rel) {
			images[e.Rel] = e.File
			continue
		}
		if strings.HasSuffix(strings.ToLower(e.Rel), ".txt") {
			// Sliced by length: TrimSuffix would miss a .Txt suffix.
			imgRel := e.Rel[:len(e.Rel)-len(".txt")]
			if HasMediaExt(imgRel) {
				sidecars[imgRel] = e.File
			}
		}
	}

	rels := slices.Sorted(maps.Keys(images))

	out := Result{Files: map[string]*zip.File{}}
	for _, rel := range rels {
		zf := images[rel]
		base := strings.TrimSuffix(path.Base(rel), path.Ext(rel))
		var tagsList []string
		if sc, ok := sidecars[rel]; ok {
			t, err := readHydrusSidecar(sc)
			if err != nil {
				logx.Warnf("hydrus import: read sidecar %q: %v", rel+".txt", err)
			}
			tagsList = t
		}
		out.Manifest.Images = append(out.Manifest.Images, ManifestImage{
			SHA256: PickValidSHA256(base),
			Path:   rel,
			Tags:   tagsList,
		})
		out.Files[rel] = zf
	}
	return out, nil
}

func readHydrusSidecar(f *zip.File) ([]string, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var tagsList []string
	sc := bufio.NewScanner(rc)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "creator:"):
			line = "artist:" + line[len("creator:"):]
		case strings.HasPrefix(line, "series:"):
			line = "copyright:" + line[len("series:"):]
		case strings.HasPrefix(line, "studio:"):
			line = "copyright:" + line[len("studio:"):]
		}
		if norm := tags.NormalizeName(line); norm != "" {
			tagsList = append(tagsList, norm)
		}
	}
	return tagsList, sc.Err()
}
