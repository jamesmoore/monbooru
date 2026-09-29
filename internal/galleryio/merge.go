package galleryio

import (
	"archive/zip"
	"bytes"
	"cmp"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// Tags are "name" in general and "category:name" elsewhere; the format
// stays this small so other software can read and write it.
type LightManifestImage struct {
	SHA256 string   `json:"sha256"`
	Path   string   `json:"path"`
	Tags   []string `json:"tags"`
}

type LightManifest struct {
	Version int                  `json:"version"`
	Images  []LightManifestImage `json:"images"`
}

func decodeLightManifest(r io.Reader) (LightManifest, error) {
	var mf LightManifest
	if err := json.NewDecoder(r).Decode(&mf); err != nil {
		return mf, fmt.Errorf("decode tags.json: %w", err)
	}
	if mf.Version != LightManifestVersion {
		return mf, fmt.Errorf("unsupported light export version %d (expected %d)", mf.Version, LightManifestVersion)
	}
	return mf, nil
}

func tagToken(category, name string) string {
	if category == "general" || category == "" {
		return name
	}
	return category + ":" + name
}

// ImportSourceNative is the tagger_name of native imports; compat imports
// use their format name.
const ImportSourceNative = "import"

func ExportGalleryLight(cx gallery.Handle, w io.Writer) error {
	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	inner, err := zw.CreateHeader(&zip.FileHeader{Name: "tags.json", Method: zip.Deflate})
	if err != nil {
		return err
	}
	if err := ExportGalleryLightManifest(cx, inner); err != nil {
		return err
	}

	if err := writeGalleryFilesToZip(zw, cx.Boundary()); err != nil {
		return err
	}
	// Close writes the central directory, so its error must reach the
	// caller; the deferred Close covers the error paths.
	return zw.Close()
}

func ExportGalleryLightManifest(cx gallery.Handle, w io.Writer) error {
	return writeLightManifest(cx.DB.Read, w, exportScope{}, nil)
}

// DownloadManifest names each image by its path in the zip, not in the gallery.
func DownloadManifest(cx gallery.Handle, names map[int64]string) ([]byte, error) {
	ids := make([]int64, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	var buf bytes.Buffer
	err := writeLightManifest(cx.DB.Read, &buf, newExportScope(ids), names)
	return buf.Bytes(), err
}

func writeLightManifest(read *sql.DB, w io.Writer, sc exportScope, names map[int64]string) error {
	bw := newJSONWriter(w)
	bw.objStart()
	bw.field("version", LightManifestVersion)
	bw.arrayStart("images")
	first := true
	err := walkLightRows(read, sc, func(id int64, sha, relPath string, missing bool, tags []string) {
		if missing {
			return
		}
		// A tag-less image ships an empty array, not null.
		if tags == nil {
			tags = []string{}
		}
		if names != nil {
			relPath = names[id]
		}
		bw.arrayItem(&first, LightManifestImage{SHA256: sha, Path: relPath, Tags: tags})
	})
	bw.arrayEnd()
	bw.objEnd()
	if err != nil {
		return err
	}
	return bw.err
}

// Grouping depends on the ORDER BY i.id: an image closes when the id changes.
func walkLightRows(read *sql.DB, sc exportScope, emit func(id int64, sha, relPath string, missing bool, tags []string)) error {
	where, args := sc.where("i.id")
	rows, err := read.Query(lightRowsQuery+where+" ORDER BY i.id, tc.name, t.name", args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var curID int64
	var curSHA, curPath string
	var curMissing bool
	var tags []string
	open := false
	for rows.Next() {
		var id int64
		var sha, folder, canonical string
		var missing bool
		var tname, tcat sql.NullString
		if err := rows.Scan(&id, &sha, &folder, &canonical, &missing, &tname, &tcat); err != nil {
			return err
		}
		if !open || id != curID {
			if open {
				emit(curID, curSHA, curPath, curMissing, tags)
			}
			curID, open = id, true
			curSHA, curMissing = sha, missing
			curPath = filepath.ToSlash(filepath.Join(folder, storedBasename(canonical)))
			tags = nil
		}
		if tname.Valid {
			tags = append(tags, tagToken(tcat.String, tname.String))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if open {
		emit(curID, curSHA, curPath, curMissing, tags)
	}
	return nil
}

// is_alias sits in the join, not a WHERE, so an alias row can't drop its image.
const lightRowsQuery = `
	SELECT i.id, i.sha256, i.folder_path, i.canonical_path, i.is_missing, t.name, tc.name
	FROM images i
	LEFT JOIN image_tags it ON it.image_id = i.id
	LEFT JOIN tags t ON t.id = it.tag_id AND t.is_alias = 0
	LEFT JOIN tag_categories tc ON tc.id = t.category_id`

func replaceFromLightArchive(manifest *zip.File, galleryFiles []*zip.File, dbPath, thumbsPath string, b *gallery.Boundary, maxFileSizeMB int) (int, error) {
	mc, err := manifest.Open()
	if err != nil {
		return 0, fmt.Errorf("open tags.json: %w", err)
	}
	mf, err := decodeLightManifest(mc)
	_ = mc.Close()
	if err != nil {
		return 0, err
	}
	files := make([]translatedFile, 0, len(galleryFiles))
	for _, f := range galleryFiles {
		files = append(files, translatedFile{
			rel:  strings.TrimPrefix(f.Name, "gallery/"),
			file: f,
		})
	}
	return ApplyLightReplace(mf, files, dbPath, thumbsPath, b, ImportSourceNative, maxFileSizeMB)
}

type translatedFile struct {
	rel  string
	file *zip.File
}

// ApplyLightReplace rebuilds the gallery from mf and files; the count is
// the files b leaves out.
func ApplyLightReplace(mf LightManifest, files []translatedFile, dbPath, thumbsPath string, b *gallery.Boundary, source string, maxFileSizeMB int) (int, error) {
	if mf.Version != LightManifestVersion {
		return 0, fmt.Errorf("unsupported light export version %d (expected %d)", mf.Version, LightManifestVersion)
	}
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024
	// Before anything is reset: a refused archive leaves the gallery whole.
	dsts := make([]string, len(files))
	for i, tf := range files {
		dst, err := SafeArchiveDest(b.Root(), tf.rel)
		if err != nil {
			return 0, fmt.Errorf("rejecting archive entry %q: %w", tf.rel, err)
		}
		if !b.Excludes(dst) {
			if err := checkEntrySize(tf.file, maxBytes); err != nil {
				return 0, err
			}
		}
		dsts[i] = dst
	}
	if err := resetDBAndThumbs(dbPath, thumbsPath); err != nil {
		return 0, err
	}
	// No files means a rebuild against what is on disk; wiping would
	// delete the files the manifest names.
	if len(files) > 0 {
		if _, err := gallery.RemoveOwned(b); err != nil {
			return 0, fmt.Errorf("wipe gallery: %w", err)
		}
	}
	leftOut := 0
	for i, tf := range files {
		if b.Excludes(dsts[i]) {
			leftOut++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dsts[i]), 0o755); err != nil {
			return leftOut, err
		}
		if err := copyZipFile(tf.file, dsts[i], maxBytes); err != nil {
			return leftOut, err
		}
	}
	return leftOut, rebuildFromLightManifest(dbPath, b, thumbsPath, mf.Images, source)
}

func resetDBAndThumbs(dbPath, thumbsPath string) error {
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	if err := os.RemoveAll(thumbsPath); err != nil {
		return fmt.Errorf("clear thumbnails: %w", err)
	}
	if err := os.MkdirAll(thumbsPath, 0o755); err != nil {
		return fmt.Errorf("recreate thumbnails dir: %w", err)
	}
	return nil
}

func replaceFromLightManifest(srcPath, dbPath, thumbsPath string, b *gallery.Boundary) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open tags.json: %w", err)
	}
	mf, err := decodeLightManifest(f)
	_ = f.Close()
	if err != nil {
		return err
	}
	if err := resetDBAndThumbs(dbPath, thumbsPath); err != nil {
		return err
	}
	return rebuildFromLightManifest(dbPath, b, thumbsPath, mf.Images, ImportSourceNative)
}

// The caller clears the old db first.
func rebuildFromLightManifest(dbPath string, b *gallery.Boundary, thumbsPath string, images []LightManifestImage, source string) error {
	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open new db: %w", err)
	}
	defer func() { _ = database.Close() }()
	if err := db.Bootstrap(database); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	ingestLightManifestEntries(database, b, thumbsPath, images, source)
	return nil
}

// An entry with no file on disk still gets an is_missing row to keep its tags.
func ingestLightManifestEntries(database *db.DB, b *gallery.Boundary, thumbsPath string, entries []LightManifestImage, source string) {
	galleryPath := b.Root()
	tagSvc := tags.New(database)
	generalID := LookupCategoryID(database, "general")
	for _, r := range entries {
		path, err := SafeArchiveDest(galleryPath, r.Path)
		if err != nil {
			logx.Warnf("light import: skipping entry %q: %v", r.Path, err)
			continue
		}
		if _, err := os.Stat(path); err != nil || b.Excludes(path) {
			imgID, err := insertMissingImageRow(database, r.SHA256, path, galleryPath, source)
			if err != nil {
				logx.Warnf("light import: record missing %q: %v", r.Path, err)
				continue
			}
			applyImportTagsToImage(database, tagSvc, imgID, r.Tags, generalID, source)
			continue
		}
		if _, err := gallery.DetectFileType(path); err != nil {
			logx.Warnf("light import: unsupported file %q: %v", r.Path, err)
			continue
		}
		img, _, err := gallery.Ingest(database, galleryPath, thumbsPath, path, source)
		if err != nil {
			logx.Warnf("light import: ingest %q: %v", r.Path, err)
			continue
		}
		applyImportTagsToImage(database, tagSvc, img.ID, r.Tags, generalID, source)
	}
}

func insertMissingImageRow(database *db.DB, sha, path, galleryPath, origin string) (int64, error) {
	ft, err := gallery.DetectFileType(path)
	if err != nil {
		return 0, err
	}
	if sha == "" {
		return 0, fmt.Errorf("manifest entry has empty sha256")
	}
	origin = cmp.Or(origin, models.OriginIngest)
	folder := gallery.FolderPath(galleryPath, path)
	tx, err := database.Write.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	err = tx.QueryRow(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size,
		                    is_missing, source_type, origin)
		 VALUES (?, ?, ?, ?, 0, 1, ?, ?)
		 ON CONFLICT(sha256) DO NOTHING
		 RETURNING id`,
		sha, path, folder, ft, models.SourceTypeNone, origin,
	).Scan(&id)
	if err == sql.ErrNoRows {
		if err := tx.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, sha).Scan(&id); err != nil {
			return 0, fmt.Errorf("fetch existing sha: %w", err)
		}
	} else if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1)`,
		id, path,
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

type mergeRecord struct {
	SHA256     string
	Tags       []string
	SourcePath string    // relative to gallery/
	zipEntry   *zip.File // when set, extract into galleryPath/<unique SourcePath>
}

type MergeResult struct {
	Added   int // images ingested from files the archive carried
	Tagged  int // images already in the gallery that the upload named
	Skipped int // records whose sha the gallery does not hold and no file backed
	LeftOut int // files the archive carried into a folder the gallery leaves out
}

// MergeGallery wipes nothing: db and json uploads only tag images matched
// by sha, and a zip also ingests the images it carries files for.
func MergeGallery(cx gallery.Handle, format string, upload io.Reader, maxFileSizeMB int) (MergeResult, error) {
	var res MergeResult
	dataDir := filepath.Dir(cx.DBPath)

	tmp, err := os.CreateTemp(dataDir, "merge-*.upload")
	if err != nil {
		return res, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := io.Copy(tmp, upload); err != nil {
		_ = tmp.Close()
		return res, fmt.Errorf("buffer upload: %w", err)
	}
	_ = tmp.Close()

	var mergeErr error
	switch format {
	case "db":
		res, mergeErr = mergeFromDB(cx, tmpPath, maxFileSizeMB)
	case "json":
		isLight, err := isLightManifestJSON(tmpPath)
		if err != nil {
			mergeErr = fmt.Errorf("inspect json: %w", err)
			break
		}
		if isLight {
			res, mergeErr = mergeFromLightJSON(cx, tmpPath, maxFileSizeMB)
		} else {
			res, mergeErr = mergeFromJSON(cx, tmpPath, maxFileSizeMB)
		}
	case "zip":
		res, mergeErr = mergeFromZip(cx, tmpPath, maxFileSizeMB)
	default:
		mergeErr = fmt.Errorf("unknown merge format %q", format)
	}
	if mergeErr == nil {
		logx.Infof("gallery: merged into %q (format=%s): +%d image(s), %d tagged, %d skipped",
			cx.Name, format, res.Added, res.Tagged, res.Skipped)
	}
	return res, mergeErr
}

// Summary states Added even at 0: the dialog promises new images, which
// only a .zip can bring.
func (r MergeResult) Summary() string {
	parts := []string{fmt.Sprintf("%d image(s) added", r.Added), fmt.Sprintf("%d tagged", r.Tagged)}
	if r.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d not in this gallery", r.Skipped))
	}
	if r.LeftOut > 0 {
		parts = append(parts, fmt.Sprintf("%d in folders this gallery leaves out", r.LeftOut))
	}
	return strings.Join(parts, ", ")
}

func mergeFromDB(cx gallery.Handle, tmpPath string, maxFileSizeMB int) (MergeResult, error) {
	records, err := readDBRecordsFromFile(tmpPath, "uploaded file is not a valid monbooru database")
	if err != nil {
		return MergeResult{}, err
	}
	return applyMergeRecords(cx, records, ImportSourceNative, maxFileSizeMB), nil
}

func readDBRecordsFromFile(path, invalidMsg string) ([]mergeRecord, error) {
	if err := validateSQLiteFile(path); err != nil {
		return nil, fmt.Errorf("%s: %w", invalidMsg, err)
	}
	src, err := db.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()
	return readDBMergeRecords(src)
}

func mergeFromJSON(cx gallery.Handle, tmpPath string, maxFileSizeMB int) (MergeResult, error) {
	f, err := os.Open(tmpPath)
	if err != nil {
		return MergeResult{}, fmt.Errorf("open json: %w", err)
	}
	defer func() { _ = f.Close() }()
	exp, err := DecodeExport(f)
	if err != nil {
		return MergeResult{}, err
	}
	return applyMergeRecords(cx, readExportMergeRecords(exp), ImportSourceNative, maxFileSizeMB), nil
}

func mergeFromLightJSON(cx gallery.Handle, tmpPath string, maxFileSizeMB int) (MergeResult, error) {
	f, err := os.Open(tmpPath)
	if err != nil {
		return MergeResult{}, fmt.Errorf("open tags.json: %w", err)
	}
	mf, err := decodeLightManifest(f)
	_ = f.Close()
	if err != nil {
		return MergeResult{}, err
	}
	records := make([]mergeRecord, 0, len(mf.Images))
	for _, img := range mf.Images {
		records = append(records, mergeRecord{SHA256: img.SHA256, Tags: img.Tags})
	}
	return applyMergeRecords(cx, records, ImportSourceNative, maxFileSizeMB), nil
}

func mergeFromZip(cx gallery.Handle, tmpPath string, maxFileSizeMB int) (MergeResult, error) {
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024
	zr, err := zip.OpenReader(tmpPath)
	if err != nil {
		return MergeResult{}, fmt.Errorf("open zip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	innerDB, innerJSON, innerLight, inGallery := classifyArchive(zr.File)
	// Keyed by the path under gallery/, which is what a light manifest names.
	galleryFiles := make(map[string]*zip.File, len(inGallery))
	for _, f := range inGallery {
		galleryFiles[strings.TrimPrefix(f.Name, "gallery/")] = f
	}

	var records []mergeRecord
	dataDir := filepath.Dir(cx.DBPath)
	switch {
	case innerLight != nil && innerDB == nil && innerJSON == nil:
		rc, err := innerLight.Open()
		if err != nil {
			return MergeResult{}, fmt.Errorf("open tags.json: %w", err)
		}
		mf, err := decodeLightManifest(rc)
		_ = rc.Close()
		if err != nil {
			return MergeResult{}, err
		}
		for _, img := range mf.Images {
			records = append(records, mergeRecord{SHA256: img.SHA256, Tags: img.Tags, SourcePath: img.Path})
		}
	case innerDB != nil:
		inner, err := extractZipEntryToTemp(innerDB, dataDir, maxBytes)
		if err != nil {
			return MergeResult{}, err
		}
		defer func() { _ = os.Remove(inner) }()
		records, err = readDBRecordsFromFile(inner, "inner db invalid")
		if err != nil {
			return MergeResult{}, err
		}
	case innerJSON != nil:
		rc, err := innerJSON.Open()
		if err != nil {
			return MergeResult{}, err
		}
		exp, err := DecodeExport(rc)
		_ = rc.Close()
		if err != nil {
			return MergeResult{}, err
		}
		records = readExportMergeRecords(exp)
	default:
		if format := detectCompatFormat(zr.File); format != "" {
			return mergeFromCompatArchive(cx, zr.File, format, maxFileSizeMB)
		}
		return MergeResult{}, fmt.Errorf("archive missing monbooru.db, monbooru.json, or tags.json")
	}

	for i := range records {
		if rel := records[i].SourcePath; rel != "" {
			if entry, ok := galleryFiles[rel]; ok {
				records[i].zipEntry = entry
			}
		}
	}
	return applyMergeRecords(cx, records, ImportSourceNative, maxFileSizeMB), nil
}

func applyMergeRecords(cx gallery.Handle, records []mergeRecord, source string, maxFileSizeMB int) MergeResult {
	var res MergeResult
	generalID := LookupCategoryID(cx.DB, "general")
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024
	bound := cx.Boundary()
	for _, r := range records {
		var imgID int64
		err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, r.SHA256).Scan(&imgID)
		if err == nil {
			applyImportTagsToImage(cx.DB, cx.TagSvc, imgID, r.Tags, generalID, source)
			res.Tagged++
			continue
		}
		if err != sql.ErrNoRows {
			logx.Warnf("merge: lookup sha %s: %v", r.SHA256, err)
			res.Skipped++
			continue
		}
		if r.zipEntry == nil || r.SourcePath == "" {
			res.Skipped++
			continue
		}
		safeBase, err := SafeArchiveDest(cx.GalleryPath, r.SourcePath)
		if err != nil {
			logx.Warnf("merge: skipping entry %q: %v", r.SourcePath, err)
			res.Skipped++
			continue
		}
		if bound.Check(safeBase) != nil {
			res.LeftOut++
			continue
		}
		// Suffix collisions within the entry's folder, not the root.
		dst := gallery.UniqueDestPath(filepath.Dir(safeBase), filepath.Base(safeBase))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			logx.Warnf("merge: mkdir for %q: %v", r.SourcePath, err)
			res.Skipped++
			continue
		}
		if err := copyZipFile(r.zipEntry, dst, maxBytes); err != nil {
			logx.Warnf("merge: extract %q: %v", r.SourcePath, err)
			res.Skipped++
			continue
		}
		if _, err := gallery.DetectFileType(dst); err != nil {
			logx.Warnf("merge: unsupported file %q: %v", r.SourcePath, err)
			_ = os.Remove(dst)
			res.Skipped++
			continue
		}
		img, isDup, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, dst, source)
		if err != nil {
			logx.Warnf("merge: ingest %q: %v", r.SourcePath, err)
			_ = os.Remove(dst)
			res.Skipped++
			continue
		}
		applyImportTagsToImage(cx.DB, cx.TagSvc, img.ID, r.Tags, generalID, source)
		// A record without a sha finds its image only once extracted.
		if isDup {
			gallery.DropDuplicateCopy(cx.DB, img.ID, dst, "merge")
			res.Tagged++
			continue
		}
		res.Added++
	}
	return res
}

// A missing row brings its tags but no file: a gallery made inside
// another still takes the outer one's tags after its sync, and an
// archive's file at that path is some other image's.
func readDBMergeRecords(src *db.DB) ([]mergeRecord, error) {
	var recs []mergeRecord
	if err := walkLightRows(src.Read, exportScope{}, func(_ int64, sha, relPath string, missing bool, tags []string) {
		if missing {
			relPath = ""
		}
		recs = append(recs, mergeRecord{SHA256: sha, SourcePath: relPath, Tags: tags})
	}); err != nil {
		return nil, err
	}
	return recs, nil
}

func readExportMergeRecords(exp Export) []mergeRecord {
	catByID := map[int64]string{}
	for _, c := range exp.TagCategories {
		catByID[c.ID] = c.Name
	}
	tagTokens := map[int64]string{}
	for _, t := range exp.Tags {
		if t.IsAlias == 1 {
			continue
		}
		tagTokens[t.ID] = tagToken(catByID[t.CategoryID], t.Name)
	}
	byImg := map[int64][]string{}
	for _, it := range exp.ImageTags {
		if tok, ok := tagTokens[it.TagID]; ok {
			byImg[it.ImageID] = append(byImg[it.ImageID], tok)
		}
	}
	var recs []mergeRecord
	for _, img := range exp.Images {
		rec := mergeRecord{SHA256: img.SHA256, Tags: byImg[img.ID]}
		if img.IsMissing == 0 {
			rec.SourcePath = filepath.ToSlash(filepath.Join(img.FolderPath, storedBasename(img.CanonicalPath)))
		}
		recs = append(recs, rec)
	}
	return recs
}

// Merged tags go through the tag service so aliases resolve and usage
// counts move. Tokens resolve before the insert transaction:
// GetOrCreateTag needs the single write connection itself.
func applyImportTagsToImage(database *db.DB, tagSvc *tags.Service, imageID int64, tokens []string, generalID int64, source string) {
	// Native keeps an unknown namespace verbatim so a round-trip is
	// lossless; foreign imports drop it.
	tagIDs, _ := resolveTokenTagIDsConf(database, tagSvc, tokens, nil, generalID, source != ImportSourceNative, source)
	if err := tagSvc.AddTagsToImageFromTagger(imageID, tagIDs, false, source); err != nil {
		logx.Warnf("import tags to image %d: %v", imageID, err)
	}
}

// confs, when not nil, pairs with tokens; the returned slices stay
// aligned as tokens drop out.
func resolveTokenTagIDsConf(database *db.DB, tagSvc *tags.Service, tokens []string, confs []*float64, generalID int64, dropUnknownNamespace bool, origin string) ([]int64, []*float64) {
	tagIDs := make([]int64, 0, len(tokens))
	outConfs := make([]*float64, 0, len(tokens))
	for i, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		catID, bareName := resolveImportTag(database, token, generalID, dropUnknownNamespace)
		t, err := tagSvc.GetOrCreateTagFrom(bareName, catID, origin)
		if err != nil {
			logx.Warnf("import tag %q: %v", token, err)
			continue
		}
		tagIDs = append(tagIDs, t.ID)
		if confs != nil {
			outConfs = append(outConfs, confs[i])
		}
	}
	return tagIDs, outConfs
}

// Returns the error: a move deletes the source only once the tags have landed.
func applyTransferTags(database *db.DB, tagSvc *tags.Service, imageID int64, groups []transferTagGroup, generalID int64) error {
	for _, g := range groups {
		// An unlabelled group was a UI add on the source side, hence "user".
		origin := g.taggerName
		origin = cmp.Or(origin, "user")
		tagIDs, confs := resolveTokenTagIDsConf(database, tagSvc, g.tokens, g.confs, generalID, false, origin)
		if err := tagSvc.AddTagsToImageFromTaggerConf(imageID, tagIDs, confs, g.isAuto, g.taggerName); err != nil {
			return fmt.Errorf("transfer tags to image %d: %w", imageID, err)
		}
	}
	return nil
}

func resolveImportTag(database *db.DB, token string, generalID int64, dropUnknownNamespace bool) (int64, string) {
	if idx := strings.Index(token, ":"); idx > 0 {
		if catID, ok, err := tags.CategoryIDByName(database, token[:idx]); ok && err == nil {
			return catID, token[idx+1:]
		}
		if dropUnknownNamespace {
			return generalID, token[idx+1:]
		}
	}
	return generalID, token
}

func LookupCategoryID(database *db.DB, name string) int64 {
	id, ok, err := tags.CategoryIDByName(database, name)
	if !ok || err != nil {
		return 0
	}
	return id
}

// The cap counts decompressed bytes, so a zip bomb can't fill the disk;
// maxBytes <= 0 means no cap.
func copyZipEntry(dst io.Writer, f *zip.File, maxBytes int64) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	var src io.Reader = rc
	if maxBytes > 0 {
		src = io.LimitReader(rc, maxBytes+1)
	}
	n, err := io.Copy(dst, src)
	if err != nil {
		return err
	}
	if maxBytes > 0 && n > maxBytes {
		return entryTooLarge(f.Name, maxBytes)
	}
	return nil
}

// A declared size can lie, so copyZipEntry still counts the bytes.
func checkEntrySize(f *zip.File, maxBytes int64) error {
	if maxBytes > 0 && f.UncompressedSize64 > uint64(maxBytes) {
		return entryTooLarge(f.Name, maxBytes)
	}
	return nil
}

func entryTooLarge(name string, maxBytes int64) error {
	return fmt.Errorf("archive entry %q exceeds per-file limit of %d bytes", name, maxBytes)
}

func copyZipFile(f *zip.File, dst string, maxBytes int64) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	err = copyZipEntry(out, f, maxBytes)
	_ = out.Close()
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
}

func extractZipEntryToTemp(f *zip.File, dataDir string, maxBytes int64) (string, error) {
	tmp, err := os.CreateTemp(dataDir, "merge-inner-*")
	if err != nil {
		return "", err
	}
	copyErr := copyZipEntry(tmp, f, maxBytes)
	_ = tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmp.Name())
		return "", copyErr
	}
	return tmp.Name(), nil
}
