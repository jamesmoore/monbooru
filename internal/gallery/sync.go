package gallery

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/tags"
)

type SyncResult struct {
	Added      int
	Removed    int
	Moved      int
	Duplicates int
	Conflicts  int
	// Excluded is the part of Removed the boundary left out rather than
	// found gone.
	Excluded    int
	Reactivated int
	Edited      int
}

func (r SyncResult) Summary() string {
	missing := fmt.Sprintf("%d missing", r.Removed)
	if r.Excluded > 0 {
		missing += fmt.Sprintf(" (%d excluded)", r.Excluded)
	}
	out := fmt.Sprintf("%d added, %s, %d moved", r.Added, missing, r.Moved)
	if r.Reactivated > 0 {
		out += fmt.Sprintf(", %d restored", r.Reactivated)
	}
	if r.Edited > 0 {
		out += fmt.Sprintf(", %d re-read", r.Edited)
	}
	if r.Conflicts > 0 {
		out += fmt.Sprintf(", %d conflicted", r.Conflicts)
	}
	return out
}

type FolderNode struct {
	Path     string
	Name     string
	Count    int
	Depth    int
	Children []FolderNode
}

type SourceLabelCount struct {
	Source string
	Count  int
}

func SourceLabelCountsQuery(database *db.DB, limit int) ([]SourceLabelCount, error) {
	return SourceLabelCountsUnderQuery(database, limit, nil)
}

type syncFileInfo struct {
	path      string
	sha256    string
	md5       string // "" when the shortcut skipped hashing; the row keeps its stored md5
	size      int64
	mtime     int64
	mtimeNano int64
}

type syncKnownEntry struct {
	size      int64
	sha256    string
	mtime     int64
	mtimeNano int64
	canonical bool
}

func (k syncKnownEntry) unchanged(size, mtime, mtimeNano int64) bool {
	if k.size != size {
		return false
	}
	if k.mtimeNano != 0 {
		return k.mtimeNano == mtimeNano
	}
	return k.mtime != 0 && k.mtime == mtime
}

type syncBySHARow struct {
	id            int64
	canonicalPath string
	isMissing     int
}

// Sync flags missing the rows whose file is gone or outside bound;
// maxFileSizeMB <= 0 means no size cap.
func Sync(ctx context.Context, database *db.DB, bound *Boundary, thumbnailsPath string, maxFileSizeMB int, naming Naming, progress func(processed, total int, message string), onChange func(), onPhash PhashSink) (SyncResult, error) {
	var result SyncResult
	galleryPath := bound.Root()

	// The walk skips unreadable directories: an unmounted root would read
	// as an empty tree and phase 3 would flag the whole library missing.
	if _, err := os.ReadDir(galleryPath); err != nil {
		return result, fmt.Errorf("gallery path is unreadable: %w", err)
	}

	progress(0, 0, "Phase 1: scanning filesystem...")
	known, err := loadKnownPaths(database)
	if err != nil {
		return result, err
	}
	// The same one level down, for a linked folder whose target went away.
	if link := danglingLinkOver(bound, known); link != "" {
		return result, fmt.Errorf("linked folder %q does not resolve", link)
	}
	found, skips, err := walkGalleryFiles(ctx, bound, int64(maxFileSizeMB)*1024*1024, known)
	if err != nil {
		return result, err
	}

	total := len(found)
	progress(0, total, "Phase 2: reconciling...")

	foundPaths := make(map[string]struct{}, total)
	for _, fi := range found {
		foundPaths[fi.path] = struct{}{}
	}

	bySHA, err := loadImagesBySHA(database)
	if err != nil {
		return result, err
	}

	// A first sync leaves files where they are: naming them would
	// reorganise a tree the operator arranged before monbooru saw it.
	adopting := len(bySHA) == 0

	var ingested []int64
	changed := 0
	for i, fi := range found {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if i%50 == 0 || i == total-1 {
			// Before the tick, so the refresh it prompts reads what changed.
			if n := result.Added + result.Moved + result.Reactivated + result.Edited; onChange != nil && n != changed {
				changed = n
				onChange()
			}
			progress(i, total, "Phase 2: reconciling...")
		}
		if id := reconcileFile(database, bound, thumbnailsPath, fi, known, bySHA, &result, onPhash); id != 0 {
			ingested = append(ingested, id)
		}
	}

	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	if adopting && !naming.Empty() && len(ingested) > 0 {
		logx.Infof("sync: first sync of this library, %d file(s) left where they are", len(ingested))
	} else {
		nameIngested(ctx, database, bound, naming, ingested, foundPaths)
	}

	toMark, excluded, err := selectImagesToMarkMissing(database, bound, foundPaths, skips)
	if err != nil {
		return result, err
	}
	removed, err := markImagesMissingChunked(ctx, database, toMark)
	result.Removed = removed
	result.Excluded = min(excluded, removed)
	if err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	// Not on an empty walk: an unmounted gallery would lose every alias row.
	if len(found) > 0 {
		if err := pruneStaleAliasPaths(ctx, database, bound, foundPaths); err != nil {
			return result, err
		}
	}

	if result.Added > 0 || result.Removed > 0 || result.Moved > 0 || result.Reactivated > 0 {
		progress(0, 0, "Recalculating tag counts...")
		tags.RecalcDB(database)
	}

	progress(0, 0, fmt.Sprintf("Done: %d added, %d missing, %d moved, %d duplicates",
		result.Added, result.Removed, result.Moved, result.Duplicates))

	return result, nil
}

// nameIngested runs before the missing sweep and adds each new path to
// foundPaths, or the sweep would flag every renamed file gone.
func nameIngested(ctx context.Context, database *db.DB, bound *Boundary, naming Naming, ids []int64, foundPaths map[string]struct{}) {
	if naming.Empty() {
		return
	}
	for _, id := range ids {
		newPath, err := naming.Apply(ctx, database, bound, id, "", "")
		if err != nil {
			logx.Warnf("sync: name image %d: %v", id, err)
			continue
		}
		if newPath != "" {
			foundPaths[newPath] = struct{}{}
		}
	}
}

// A deleted folder is not one of these: its rows are meant to go missing.
func danglingLinkOver(bound *Boundary, known map[string]syncKnownEntry) string {
	galleryPath := bound.Root()
	checked := map[string]struct{}{}
	for path := range known {
		if bound.Excludes(path) {
			continue
		}
		for dir := filepath.Dir(path); dir != galleryPath && PathInside(galleryPath, dir); dir = filepath.Dir(dir) {
			if _, seen := checked[dir]; seen {
				break
			}
			checked[dir] = struct{}{}
			info, err := os.Lstat(dir)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				continue
			}
			if _, err := os.Stat(dir); err != nil {
				return dir
			}
		}
	}
	return ""
}

func loadKnownPaths(database *db.DB) (map[string]syncKnownEntry, error) {
	known := map[string]syncKnownEntry{}
	rows, err := database.Read.Query(
		`SELECT ip.path, i.file_size, i.sha256, ip.mtime_unix, ip.mtime_nsec, ip.is_canonical
		   FROM image_paths ip JOIN images i ON i.id = ip.image_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("preloading known paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p, sha string
		var sz, mt, mtNano int64
		var canonical bool
		if err := rows.Scan(&p, &sz, &sha, &mt, &mtNano, &canonical); err != nil {
			return nil, fmt.Errorf("scanning known paths: %w", err)
		}
		known[p] = syncKnownEntry{size: sz, sha256: sha, mtime: mt, mtimeNano: mtNano, canonical: canonical}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating known paths: %w", err)
	}
	return known, nil
}

// walkSkips is what a walk passed over without reading: those rows keep
// their state rather than read as gone.
type walkSkips struct {
	files map[string]struct{}
	dirs  []string
}

func (s walkSkips) covers(path string) bool {
	if _, ok := s.files[path]; ok {
		return true
	}
	for _, dir := range s.dirs {
		if PathInside(dir, path) {
			return true
		}
	}
	return false
}

func walkGalleryFiles(ctx context.Context, bound *Boundary, maxBytes int64, known map[string]syncKnownEntry) ([]syncFileInfo, walkSkips, error) {
	var found []syncFileInfo
	skips := walkSkips{files: map[string]struct{}{}}
	err := WalkTree(bound, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				skips.dirs = append(skips.dirs, path)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		if _, typeErr := DetectFileType(path); typeErr != nil {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || (maxBytes > 0 && info.Size() > maxBytes) {
			skips.files[path] = struct{}{}
			return nil
		}
		mtimeUnix, mtimeNano := info.ModTime().Unix(), info.ModTime().UnixNano()
		var hash, sum string
		if k, ok := known[path]; ok && k.unchanged(info.Size(), mtimeUnix, mtimeNano) {
			hash = k.sha256
		} else {
			h, m, hashErr := hashFileDigests(path)
			if hashErr != nil {
				logx.Warnf("hash failed for %q: %v", path, hashErr)
				skips.files[path] = struct{}{}
				return nil
			}
			hash, sum = h, m
			claimOwnership(bound.Root(), path)
		}
		found = append(found, syncFileInfo{path: path, sha256: hash, md5: sum, size: info.Size(), mtime: mtimeUnix, mtimeNano: mtimeNano})
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, skips, ctx.Err()
		}
		return nil, skips, fmt.Errorf("walking gallery: %w", err)
	}
	return found, skips, nil
}

// One full scan beats a SELECT per walked file.
func loadImagesBySHA(database *db.DB) (map[string]syncBySHARow, error) {
	bySHA := map[string]syncBySHARow{}
	rows, err := database.Read.Query(
		`SELECT id, sha256, canonical_path, is_missing FROM images`,
	)
	if err != nil {
		return nil, fmt.Errorf("preloading SHA index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r syncBySHARow
		var sha string
		if err := rows.Scan(&r.id, &sha, &r.canonicalPath, &r.isMissing); err != nil {
			return nil, fmt.Errorf("scanning SHA index: %w", err)
		}
		bySHA[sha] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating SHA index: %w", err)
	}
	return bySHA, nil
}

// Every branch updates bySHA, or a later file with the same SHA acts on
// the stale entry: a moved image would be moved again.
func reconcileFile(database *db.DB, bound *Boundary, thumbnailsPath string, fi syncFileInfo, known map[string]syncKnownEntry, bySHA map[string]syncBySHARow, result *SyncResult, onPhash PhashSink) int64 {
	if k, knownPath := known[fi.path]; knownPath && k.sha256 != fi.sha256 && !k.canonical {
		// A rewritten copy: its image keeps its own file, and the path is
		// classified afresh, as the watcher's ingest does.
		if _, err := database.Write.Exec(`DELETE FROM image_paths WHERE path = ? AND is_canonical = 0`, fi.path); err != nil {
			logx.Warnf("sync: drop the rewritten copy %q: %v", fi.path, err)
			return 0
		}
		delete(known, fi.path)
	}
	if k, knownPath := known[fi.path]; knownPath && k.sha256 != fi.sha256 {
		// images.sha256 is UNIQUE, so the UPDATE would fail on every
		// sync; which row the operator wants is not ours to guess.
		if other, held := bySHA[fi.sha256]; held {
			logx.Warnf("sync: %q was rewritten with the bytes of image %d at %q; row left as it is",
				fi.path, other.id, other.canonicalPath)
			result.Conflicts++
			return 0
		}
		phash, err := applyInPlaceEdit(database, thumbnailsPath, fi.path, fi.sha256, fi.md5, fi.mtime, fi.mtimeNano, fi.size)
		if err != nil {
			logx.Warnf("sync: in-place edit %q: %v", fi.path, err)
			return 0
		}
		result.Edited++
		var imgID int64
		if err := database.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, fi.sha256).Scan(&imgID); err == nil {
			bySHA[fi.sha256] = syncBySHARow{id: imgID, canonicalPath: fi.path, isMissing: 0}
			onPhash.Stored(imgID, phash)
		}
		delete(bySHA, k.sha256)
		known[fi.path] = syncKnownEntry{size: fi.size, sha256: fi.sha256, mtime: fi.mtime}
		return 0
	}

	row, ok := bySHA[fi.sha256]
	if !ok {
		return reconcileNewFile(database, bound.Root(), thumbnailsPath, fi, bySHA, result, onPhash)
	}
	reconcileExistingSHA(database, bound, fi, row, bySHA, known, result)
	return 0
}

func reconcileNewFile(database *db.DB, galleryPath, thumbnailsPath string, fi syncFileInfo, bySHA map[string]syncBySHARow, result *SyncResult, onPhash PhashSink) int64 {
	img, _, ingestErr := ingestWithHash(database, galleryPath, thumbnailsPath, fi.path, fi.sha256, fi.md5, "")
	if ingestErr != nil {
		logx.Warnf("ingest failed for %q: %v", fi.path, ingestErr)
		return 0
	}
	result.Added++
	if img == nil {
		return 0
	}
	onPhash.Stored(img.ID, img.Phash)
	bySHA[fi.sha256] = syncBySHARow{id: img.ID, canonicalPath: fi.path, isMissing: 0}
	return img.ID
}

func reconcileExistingSHA(database *db.DB, bound *Boundary, fi syncFileInfo, row syncBySHARow, bySHA map[string]syncBySHARow, known map[string]syncKnownEntry, result *SyncResult) {
	galleryPath := bound.Root()
	if _, wErr := database.Write.Exec(
		`UPDATE image_paths SET mtime_unix = ?, mtime_nsec = ? WHERE path = ?`, fi.mtime, fi.mtimeNano, fi.path,
	); wErr != nil {
		logx.Warnf("sync: persist mtime for %q: %v", fi.path, wErr)
	}

	if row.canonicalPath == fi.path {
		if row.isMissing == 1 {
			reactivateImage(database, row.id)
			result.Reactivated++
		}
		return
	}

	if k, knownAlias := known[fi.path]; knownAlias && k.sha256 == fi.sha256 {
		if canonicalGone(bound, row.canonicalPath) {
			promoteAliasToCanonical(database, galleryPath, fi.path, row)
			bySHA[fi.sha256] = syncBySHARow{id: row.id, canonicalPath: fi.path, isMissing: 0}
			result.Moved++
		} else if row.isMissing == 1 {
			reactivateImage(database, row.id)
			result.Reactivated++
		}
		return
	}

	if canonicalGone(bound, row.canonicalPath) {
		moveCanonical(database, galleryPath, fi.path, row)
		bySHA[fi.sha256] = syncBySHARow{id: row.id, canonicalPath: fi.path, isMissing: 0}
		result.Moved++
		return
	}
	if _, wErr := database.Write.Exec(
		`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical, mtime_unix, mtime_nsec) VALUES (?, ?, 0, ?, ?)`,
		row.id, fi.path, fi.mtime, fi.mtimeNano,
	); wErr != nil {
		logx.Warnf("sync: insert alias path %d: %v", row.id, wErr)
	}
	result.Duplicates++
}

func canonicalGone(bound *Boundary, path string) bool {
	if bound.Check(path) != nil {
		return true
	}
	_, err := os.Stat(path)
	return err != nil
}

func reactivateImage(database *db.DB, imageID int64) {
	if _, wErr := database.Write.Exec(`UPDATE images SET is_missing = 0 WHERE id = ?`, imageID); wErr != nil {
		logx.Warnf("sync: reactivate %d: %v", imageID, wErr)
	}
}

func promoteAliasToCanonical(database *db.DB, galleryPath, newCanonical string, row syncBySHARow) {
	if err := repointCanonical(database.Write, row.id, newCanonical,
		FolderPath(galleryPath, newCanonical), row.canonicalPath); err != nil {
		logx.Warnf("sync: promote alias %d: %v", row.id, err)
	}
}

func moveCanonical(database *db.DB, galleryPath, newCanonical string, row syncBySHARow) {
	if err := repointCanonical(database.Write, row.id, newCanonical,
		FolderPath(galleryPath, newCanonical), row.canonicalPath); err != nil {
		logx.Warnf("sync: move %d: %v", row.id, err)
	}
}

func selectImagesToMarkMissing(database *db.DB, bound *Boundary, foundPaths map[string]struct{}, skips walkSkips) ([]int64, int, error) {
	rows, err := database.Read.Query(
		`SELECT id, canonical_path FROM images WHERE is_missing = 0`,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("querying existing images: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var toMark []int64
	excluded := 0
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			logx.Warnf("sync: scan existing image row: %v", err)
			continue
		}
		if _, seen := foundPaths[path]; !seen && !skips.covers(path) {
			toMark = append(toMark, id)
			if bound.Excludes(path) {
				excluded++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return toMark, excluded, fmt.Errorf("iterating existing images: %w", err)
	}
	return toMark, excluded, nil
}

func markImagesMissingChunked(ctx context.Context, database *db.DB, ids []int64) (int, error) {
	marked := 0
	err := db.Chunked(ids, 500, func(chunk []int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		placeholders, args := db.InPlaceholders(chunk)
		res, wErr := database.Write.Exec(
			`UPDATE images SET is_missing = 1 WHERE id IN (`+placeholders+`)`, args...,
		)
		if wErr != nil {
			logx.Warnf("sync: mark missing chunk: %v", wErr)
			return fmt.Errorf("mark missing chunk: %w", wErr)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			marked += int(n)
		}
		return nil
	})
	return marked, err
}

// A path the walk skipped (size cap, unreadable) keeps its row until its
// file is gone. An excluded path goes even when present: that copy is
// another gallery's.
func pruneStaleAliasPaths(ctx context.Context, database *db.DB, bound *Boundary, foundPaths map[string]struct{}) error {
	type aliasPath struct {
		id              int64
		path, canonical string
	}
	// Collected before any stat so the read cursor is not held open
	// across the syscalls.
	aliases, err := db.QueryAll(database.Read, func(rows *sql.Rows) (aliasPath, error) {
		var a aliasPath
		err := rows.Scan(&a.id, &a.path, &a.canonical)
		return a, err
	}, `SELECT ip.id, ip.path, i.canonical_path FROM image_paths ip JOIN images i ON i.id = ip.image_id WHERE ip.is_canonical = 0`)
	if err != nil {
		return fmt.Errorf("listing alias paths: %w", err)
	}
	var staleIDs []int64
	for _, a := range aliases {
		if _, ok := foundPaths[a.path]; ok {
			continue
		}
		if bound.Check(a.path) != nil {
			staleIDs = append(staleIDs, a.id)
			continue
		}
		// An unwalked path onto the canonical's own file is a second name
		// the watcher took through a link the walk refuses.
		info, statErr := os.Stat(a.path)
		if os.IsNotExist(statErr) || statErr == nil && sameFile(info, a.canonical) {
			staleIDs = append(staleIDs, a.id)
		}
	}

	return db.Chunked(staleIDs, 500, func(chunk []int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		placeholders, args := db.InPlaceholders(chunk)
		if _, wErr := database.Write.Exec(
			`DELETE FROM image_paths WHERE id IN (`+placeholders+`)`, args...,
		); wErr != nil {
			return fmt.Errorf("prune alias paths chunk: %w", wErr)
		}
		return nil
	})
}

func sameFile(info os.FileInfo, path string) bool {
	other, err := os.Stat(path)
	return err == nil && os.SameFile(info, other)
}

// applyInPlaceEdit keeps the row's id, so its tags survive the rewrite.
func applyInPlaceEdit(database *db.DB, thumbnailsPath, path, newSHA, newMD5 string, newMtime, newMtimeNano, newSize int64) (*int64, error) {
	var imageID int64
	if err := database.Read.QueryRow(
		`SELECT image_id FROM image_paths WHERE path = ?`, path,
	).Scan(&imageID); err != nil {
		return nil, fmt.Errorf("locate image for path %q: %w", path, err)
	}

	// Sniffed, as a rewrite can change the type under the same name;
	// bytes that are no longer media leave the row as it was.
	fileType, err := detectMagicType(path)
	if err != nil {
		return nil, fmt.Errorf("contents of %q are not a supported media type: %w", path, err)
	}

	var imgWidth, imgHeight *int
	var pageCount *int
	var durationSec *float64
	if fileType == "cbz" {
		archive, openErr := OpenManga(path)
		if openErr == nil {
			if w, h, dimErr := archive.coverDimensions(); dimErr == nil {
				imgWidth, imgHeight = &w, &h
			}
			pcVal := len(archive.Pages)
			pageCount = &pcVal
			_ = archive.Close()
		}
	} else if IsVideoType(fileType) {
		if w, h, ok := ProbeVideoDimensions(path); ok {
			imgWidth, imgHeight = &w, &h
		}
		if d, ok := ProbeDurationSeconds(path); ok {
			durationSec = &d
		}
	} else {
		imgWidth, imgHeight = stillDimensions(path, fileType)
	}
	sdMeta, comfyMeta, sourceType := extractGenerationMeta(path, fileType)

	tx, err := database.Write.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin in-place edit tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		// duration_seconds is written for every type, or a former video
		// keeps matching the duration: filter.
		`UPDATE images SET sha256 = ?, md5 = ?, file_size = ?, file_type = ?, width = ?, height = ?, page_count = ?, duration_seconds = ?, source_type = ?, phash = NULL WHERE id = ?`,
		newSHA, newMD5, newSize, fileType, toNullInt(imgWidth), toNullInt(imgHeight), toNullInt(pageCount), toNullFloat(durationSec), sourceType, imageID,
	); err != nil {
		return nil, fmt.Errorf("update images row: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET mtime_unix = ?, mtime_nsec = ? WHERE path = ?`, newMtime, newMtimeNano, path,
	); err != nil {
		return nil, fmt.Errorf("update image_paths mtime: %w", err)
	}
	// The recorded lookup misses are about bytes this row no longer has.
	if err := lookup.DeleteForImage(tx, imageID); err != nil {
		return nil, fmt.Errorf("clear lookup history: %w", err)
	}
	if err := ReplaceGenerationMetadata(context.Background(), tx, imageID, sdMeta, comfyMeta); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit in-place edit: %w", err)
	}

	// The reader serves cached pages without revalidating them, so the
	// old contents' cache must go.
	if fileType == "cbz" {
		removeMangaCache(thumbnailsPath, imageID)
	}

	phash := regenerateDerived(database, thumbnailsPath, path, imageID, fileType, "in-place edit")
	if err := ApplyMetaTags(database, thumbnailsPath, imageID); err != nil {
		logx.Warnf("in-place edit: meta tags for %q: %v", path, err)
	}
	logx.Infof("in-place edit: image id=%d at %q now carries sha %s", imageID, path, newSHA)
	return phash, nil
}

// FolderTree's counts include every descendant's, and a folder holding
// only subfolders still gets a node.
func FolderTree(database *db.DB) ([]FolderNode, error) {
	flat, err := db.QueryAll(database.Read, scanFolderRow,
		`SELECT COALESCE(folder_path, ''), COUNT(*) FROM images WHERE is_missing=0 GROUP BY folder_path ORDER BY folder_path`)
	if err != nil {
		return nil, err
	}
	return buildFolderTree(flat), nil
}

func buildFolderTree(flat []folderCount) []FolderNode {
	known := map[string]bool{"": true}
	for _, fc := range flat {
		known[fc.path] = true
	}
	var toAdd []folderCount
	for _, fc := range flat {
		if fc.path == "" {
			continue
		}
		segments := strings.Split(fc.path, "/")
		for i := 1; i < len(segments); i++ {
			ancestor := strings.Join(segments[:i], "/")
			if !known[ancestor] {
				known[ancestor] = true
				toAdd = append(toAdd, folderCount{path: ancestor, count: 0})
			}
		}
	}
	flat = append(flat, toAdd...)

	// Pointer-tree intermediate so parent-child wiring survives mutations.
	type pnode struct {
		FolderNode
		children []*pnode
	}

	rootP := &pnode{FolderNode: FolderNode{Path: "", Name: "(root)", Depth: 0}}
	pnodeMap := map[string]*pnode{"": rootP}

	// Sort lexicographically so parents always exist before children.
	slices.SortFunc(flat, func(a, b folderCount) int {
		return cmp.Compare(a.path, b.path)
	})

	for _, fc := range flat {
		if fc.path == "" {
			rootP.Count = fc.count
			continue
		}
		// folder_path is "/"-separated on every platform, which
		// filepath.Dir is not.
		name := fc.path
		parentPath := ""
		if i := strings.LastIndex(fc.path, "/"); i >= 0 {
			name = fc.path[i+1:]
			parentPath = fc.path[:i]
		}
		n := &pnode{FolderNode: FolderNode{
			Path:  fc.path,
			Name:  name,
			Count: fc.count,
			Depth: strings.Count(fc.path, "/") + 1,
		}}
		pnodeMap[fc.path] = n

		parent, ok := pnodeMap[parentPath]
		if !ok {
			parent = rootP
		}
		parent.children = append(parent.children, n)
	}

	var rollup func(p *pnode)
	rollup = func(p *pnode) {
		for _, c := range p.children {
			rollup(c)
			p.Count += c.Count
		}
	}
	rollup(rootP)

	var toValue func(p *pnode) FolderNode
	toValue = func(p *pnode) FolderNode {
		n := p.FolderNode
		for _, c := range p.children {
			n.Children = append(n.Children, toValue(c))
		}
		return n
	}

	return []FolderNode{toValue(rootP)}
}
