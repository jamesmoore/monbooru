package compatibility

import (
	"archive/zip"
	"cmp"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/monbooru/monbooru/internal/tags"
)

func init() {
	Register(Provider{
		Name:      "blombooru",
		Detect:    detectBlombooru,
		Translate: translateBlombooru,
	})
}

type blombooruBackup struct {
	Version int                    `json:"version"`
	Type    string                 `json:"type"`
	Media   []blombooruBackupMedia `json:"media"`
}

type blombooruBackupMedia struct {
	Filename    string   `json:"filename"`
	Hash        string   `json:"hash"`
	Tags        []string `json:"tags"`
	ArchivePath string   `json:"archive_path"`
}

func detectBlombooru(entries []NormalizedEntry) bool {
	hasBackup, hasMedia := false, false
	for _, e := range entries {
		switch {
		case e.Rel == "backup.json":
			hasBackup = true
		case strings.HasPrefix(e.Rel, "media/"):
			hasMedia = true
		}
		if hasBackup && hasMedia {
			return true
		}
	}
	return false
}

func translateBlombooru(entries []NormalizedEntry) (Result, error) {
	var backupFile, tagsCSV *zip.File
	media := map[string]*zip.File{}
	for _, e := range entries {
		switch {
		case e.Rel == "backup.json":
			backupFile = e.File
		case e.Rel == "tags.csv":
			tagsCSV = e.File
		case strings.HasPrefix(e.Rel, "media/"):
			media[strings.TrimPrefix(e.Rel, "media/")] = e.File
		}
	}
	if backupFile == nil {
		return Result{}, fmt.Errorf("blombooru archive missing backup.json")
	}

	catByTag, err := readBlombooruTagsCSV(tagsCSV)
	if err != nil {
		return Result{}, err
	}

	rc, err := backupFile.Open()
	if err != nil {
		return Result{}, fmt.Errorf("open backup.json: %w", err)
	}
	var bb blombooruBackup
	err = json.NewDecoder(rc).Decode(&bb)
	_ = rc.Close()
	if err != nil {
		return Result{}, fmt.Errorf("decode backup.json: %w", err)
	}

	out := Result{Files: map[string]*zip.File{}}
	for _, m := range bb.Media {
		rel := strings.TrimPrefix(m.ArchivePath, "media/")
		rel = cmp.Or(rel, m.Filename)
		if rel == "" {
			continue
		}
		out.Manifest.Images = append(out.Manifest.Images, ManifestImage{
			SHA256: PickValidSHA256(m.Hash),
			Path:   rel,
			Tags:   blombooruTagTokens(m.Tags, catByTag),
		})
		if zf, ok := media[rel]; ok {
			out.Files[rel] = zf
		}
	}
	return out, nil
}

func blombooruTagTokens(names []string, catByTag map[string]string) []string {
	out := make([]string, 0, len(names))
	for _, t := range names {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		cat := catByTag[t]
		name := tags.NormalizeName(t)
		if name == "" {
			continue
		}
		if cat != "" && cat != "general" {
			out = append(out, cat+":"+name)
		} else {
			out = append(out, name)
		}
	}
	return out
}

// Rows are name, category_id[, ...]; a malformed row ends the read,
// keeping the rows before it.
func readBlombooruTagsCSV(tagsFile *zip.File) (map[string]string, error) {
	if tagsFile == nil {
		return map[string]string{}, nil
	}
	rc, err := tagsFile.Open()
	if err != nil {
		return nil, fmt.Errorf("open tags.csv: %w", err)
	}
	defer func() { _ = rc.Close() }()
	out := map[string]string{}
	r := csv.NewReader(rc)
	r.FieldsPerRecord = -1
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, nil
		}
		if len(rec) < 2 {
			continue
		}
		name := strings.TrimSpace(rec[0])
		if name == "" {
			continue
		}
		out[name] = blombooruCategoryByID(strings.TrimSpace(rec[1]))
	}
	return out, nil
}

func blombooruCategoryByID(id string) string {
	switch id {
	case "0":
		return "general"
	case "1":
		return "artist"
	case "3":
		return "copyright"
	case "4":
		return "character"
	case "5":
		return "meta"
	}
	return "general"
}
