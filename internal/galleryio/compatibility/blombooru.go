package compatibility

import (
	"archive/zip"
	"cmp"
	"encoding/csv"
	"encoding/json"
	"errors"
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

	rc, err := openDocument(backupFile)
	if err != nil {
		return Result{}, err
	}
	var bb blombooruBackup
	err = json.NewDecoder(rc).Decode(&bb)
	_ = rc.Close()
	if err != nil {
		return Result{}, fmt.Errorf("decode backup.json: %w", err)
	}

	out := Result{Files: map[string]*zip.File{}}
	for _, m := range bb.Media {
		rel := cmp.Or(strings.TrimPrefix(m.ArchivePath, "media/"), m.Filename)
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
		out = append(out, TagToken(cat, name))
	}
	return out
}

// Rows are name, category_id[, ...]; a malformed row ends the read,
// keeping the rows before it, while a file over the cap is refused.
func readBlombooruTagsCSV(tagsFile *zip.File) (map[string]string, error) {
	if tagsFile == nil {
		return map[string]string{}, nil
	}
	rc, err := openDocument(tagsFile)
	if err != nil {
		return nil, err
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
		if errors.Is(err, errDocumentTooLarge) {
			return nil, err
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
		out[name] = cmp.Or(blombooruCategories[strings.TrimSpace(rec[1])], "general")
	}
	return out, nil
}

var blombooruCategories = map[string]string{
	"1": "artist",
	"3": "copyright",
	"4": "character",
	"5": "meta",
}
