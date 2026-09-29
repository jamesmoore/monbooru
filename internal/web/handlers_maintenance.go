package web

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
	meta "github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/tagger"
)

type missingRow struct {
	id   int64
	path string
}

func (s *Server) pruneMissingImagesPost(w http.ResponseWriter, r *http.Request) {
	missing, err := db.QueryAll(s.db().Read, func(rows *sql.Rows) (missingRow, error) {
		var m missingRow
		err := rows.Scan(&m.id, &m.path)
		return m, err
	}, `SELECT id, canonical_path FROM images WHERE is_missing = 1`)
	if err != nil {
		writeInlineFlash(w, "err", "Error: "+err.Error())
		return
	}
	if len(missing) == 0 {
		writeInlineFlash(w, "ok", "Removed 0 missing image(s).")
		return
	}

	if !s.startJob(w, models.JobTypeDelete) {
		return
	}
	thumbnailsPath := s.thumbnailsPath()
	tagSvc := s.tagSvc()
	active := s.active()
	onDelete := s.onImagesDeleteCallback()
	go func() {
		ctx := s.jobs.Context()
		// The flag is also set for a path the gallery leaves out, or a file
		// sync skipped; only a file gone from disk takes its row.
		s.jobs.Update(0, len(missing), "checking files…")
		var ids []int64
		for _, m := range missing {
			if _, err := os.Stat(m.path); os.IsNotExist(err) {
				ids = append(ids, m.id)
			}
		}
		kept := len(missing) - len(ids)
		total := len(ids)
		s.jobs.Update(0, total, "pruning…")
		done := 0
		removed := 0
		affectedTags, processed, cancelled, err := tagSvc.ChunkedDeleteWithTagRecalc(
			ctx, ids, "", nil,
			func(tx *sql.Tx, chunk []int64, placeholders string, args []any) error {
				if onDelete != nil {
					if err := onDelete(tx, chunk); err != nil {
						return err
					}
				}
				res, err := tx.Exec(`DELETE FROM images WHERE id IN (`+placeholders+`)`, args...)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					removed += int(n)
				}
				return nil
			},
			func(chunk []int64) {
				for _, id := range chunk {
					gallery.RemoveImageArtifacts(thumbnailsPath, id, "")
				}
				done += len(chunk)
				s.jobs.Update(done, total, "pruning…")
			},
		)
		if err == nil {
			if len(affectedTags) > 0 {
				s.jobs.Update(processed, total, "reconciling tag counts…")
				if err := tagSvc.RecalcIDs(affectedTags); err != nil {
					logx.Warnf("prune-missing recalc IDs: %v", err)
				}
			}
			if removed > 0 && active != nil {
				active.InvalidateCaches()
			}
		}
		summary := fmt.Sprintf("Removed %d missing image(s).", removed)
		if kept > 0 {
			summary = fmt.Sprintf("Removed %d missing image(s); kept %d whose file is still on disk.", removed, kept)
		}
		s.finishJob(err, cancelled, fmt.Sprintf("prune cancelled (%d/%d removed)", removed, total), summary)
	}()
	writeInlineFlash(w, "ok", "Prune started.")
}

func (s *Server) pruneOrphanedThumbnailsPost(w http.ResponseWriter, r *http.Request) {
	cx := s.active()
	if cx == nil {
		writeInlineFlash(w, "err", "No active gallery.")
		return
	}
	if !s.startJob(w, models.JobTypePruneThumbs) {
		return
	}
	go func() {
		ctx := s.jobs.Context()
		removed, processed, total, err := s.runOrphanSweep(ctx, cx)
		s.finishJob(err, ctx.Err() != nil,
			fmt.Sprintf("orphan sweep cancelled (%d/%d scanned, %d removed)", processed, total, removed),
			fmt.Sprintf("Removed %d orphaned thumbnail(s).", removed))
	}()
	writeInlineFlash(w, "ok", "Thumbnail prune started.")
}

// emptyFolderListCap bounds the review list; past it the operator removes
// what is listed and scans again.
const emptyFolderListCap = 2000

func (s *Server) emptyFoldersScanPost(w http.ResponseWriter, r *http.Request) {
	cx := s.active()
	// Every outcome but the list clears the list slot, so an old list
	// never stays under a new flash.
	if cx == nil || cx.Degraded {
		writeFlashOOB(w, "flash-empty-folders", "", "")
		writeInlineFlash(w, "err", "Gallery path is unreadable.")
		return
	}
	dirs, err := gallery.ScanEmptyDirs(cx.Boundary())
	if err != nil {
		writeFlashOOB(w, "flash-empty-folders", "", "")
		writeInlineFlash(w, "err", "Error: "+err.Error())
		return
	}
	if len(dirs) == 0 {
		writeFlashOOB(w, "flash-empty-folders", "", "")
		writeInlineFlash(w, "ok", "No empty folders.")
		return
	}
	s.renderTemplate(w, "partials/empty_folders.html", map[string]any{
		"Dirs":      dirs[:min(len(dirs), emptyFolderListCap)],
		"Withheld":  max(len(dirs)-emptyFolderListCap, 0),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	})
}

// Holds a job slot so a concurrent move cannot lose a folder it just created.
func (s *Server) emptyFoldersRemovePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	paths := r.Form["folder"]
	if len(paths) == 0 {
		writeInlineFlash(w, "err", "No folders selected.")
		return
	}
	if !s.startJob(w, models.JobTypePruneDirs) {
		return
	}
	removed := gallery.RemoveEmptyDirs(s.boundary(), paths)
	s.jobs.Complete(fmt.Sprintf("Removed %d empty folder(s).", removed))
	kept := ""
	if n := len(paths) - removed; n > 0 {
		kept = fmt.Sprintf(", %d no longer empty", n)
	}
	writeFlashOOB(w, "flash-empty-folders", "", "")
	writeInlineFlash(w, "ok", fmt.Sprintf("Removed %d empty folder(s)%s.", removed, kept))
}

func (s *Server) recalcTagsPost(w http.ResponseWriter, r *http.Request) {
	updated, err := s.tagSvc().RecalcCount()
	s.active().InvalidateCaches()
	if err != nil {
		writeInlineFlash(w, "err", fmt.Sprintf("Recalc partially completed (%d updated): %s", updated, err.Error()))
		return
	}
	writeInlineFlash(w, "ok", fmt.Sprintf("Recalculated %d tag count(s).", updated))
}

func (s *Server) tagCategoryConflictsPost(w http.ResponseWriter, r *http.Request) {
	n, err := s.tagSvc().ConflictsCount()
	if err != nil {
		writeInlineFlash(w, "err", "Error: "+err.Error())
		return
	}
	if n == 0 {
		writeInlineFlash(w, "ok", "No tags share a name across categories.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w,
		`<div class="flash flash-ok"><strong>%d</strong> tag%s share a name across categories - <a href="/tags?conflicts=1">review them on the Tags page</a>.</div>`,
		n, plural(n))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (s *Server) findFoldedDuplicatesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if !s.startJob(w, models.JobTypeFold) {
		return
	}
	go func() {
		n, err := s.tagSvc().ScanFoldedDuplicates()
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(fmt.Sprintf("Found %d folded duplicate(s).", n))
	}()
	writeInlineFlash(w, "ok", "Folded-duplicate scan started.")
}

const duplicatesFragmentCap = 100

func (s *Server) duplicatesListHandler(w http.ResponseWriter, r *http.Request) {
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/relations#file-duplicates", http.StatusSeeOther)
		return
	}
	// Ceiling-filtered: this surface prints paths and ids, and [promote]
	// acts on them.
	aliases, err := duplicatePaths(r, s.active())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	shown := aliases[:min(len(aliases), duplicatesFragmentCap)]

	s.renderTemplate(w, "partials/duplicates_list.html", map[string]any{
		"Aliases":   shown,
		"Withheld":  len(aliases) - len(shown),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	})
}

func (s *Server) removeDuplicatesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	// all=true is required for the whole set, so a stray POST cannot wipe
	// every alias file.
	selected := r.Form["path_id"]
	allFlag := r.FormValue("all") == "true"
	if len(selected) == 0 && !allFlag {
		writeInlineFlash(w, "err", "No duplicate paths selected.")
		return
	}

	var query string
	var args []any
	if allFlag {
		// Ceiling-filtered, so delete-all cannot take aliases the
		// operator cannot see.
		query = `
			SELECT ip.id, ip.path, i.canonical_path, i.file_size
			FROM image_paths ip
			JOIN images i ON i.id = ip.image_id
			WHERE ip.is_canonical = 0`
		if where, wargs := resolveCeiling(r, s.active()).WhereOne("i.id"); where != "" {
			query += ` AND ` + where
			args = append(args, wargs...)
		}
	} else {
		// Only non-canonical paths: this endpoint must never remove an
		// image's canonical file.
		ids := make([]int64, 0, len(selected))
		for _, s := range selected {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			writeInlineFlash(w, "err", "No valid path_ids in request.")
			return
		}
		var placeholders string
		placeholders, args = db.InPlaceholders(ids)
		query = `SELECT ip.id, ip.path, i.canonical_path, i.file_size FROM image_paths ip
			 JOIN images i ON i.id = ip.image_id
			 WHERE ip.is_canonical = 0 AND ip.id IN (` + placeholders + `)`
	}

	type pathRow struct {
		ID        int64
		Path      string
		Canonical string
		Size      int64
	}
	paths, err := db.QueryAll(s.db().Read, func(rows *sql.Rows) (pathRow, error) {
		var p pathRow
		err := rows.Scan(&p.ID, &p.Path, &p.Canonical, &p.Size)
		return p, err
	}, query, args...)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}

	if len(paths) == 0 {
		writeInlineFlash(w, "ok", "Removed 0 duplicate path(s).")
		return
	}

	if !s.startJob(w, models.JobTypeDelete) {
		return
	}
	bound := s.boundary()
	go func() {
		ctx := s.jobs.Context()
		total := len(paths)
		removed := 0
		const chunkSize = 500
		pathIDs := make([]int64, len(paths))
		byID := make(map[int64]pathRow, len(paths))
		for i, p := range paths {
			pathIDs[i] = p.ID
			byID[p.ID] = p
		}
		_, cancelled, err := jobs.Chunked(ctx, s.jobs, pathIDs, chunkSize, "removing", func(chunk []int64) error {
			if err := gallery.DeleteAliasPaths(s.db(), chunk); err != nil {
				logx.Warnf("remove duplicates chunk delete: %v", err)
				return err
			}
			for _, id := range chunk {
				row := byID[id]
				if row.Path == "" {
					removed++
					continue
				}
				// Its row goes, but the copy is another gallery's file,
				// not a duplicate the listing showed.
				if bound.Excludes(row.Path) {
					continue
				}
				if err := unlinkAliasFile(bound, row.Path, row.Canonical, row.Size); err != nil {
					logx.Warnf("remove duplicate %q: %v", row.Path, err)
				}
				removed++
			}
			return nil
		})
		s.finishJob(err, cancelled, fmt.Sprintf("remove duplicates cancelled (%d/%d)", removed, total), fmt.Sprintf("Removed %d duplicate path(s).", removed))
	}()
	writeInlineFlash(w, "ok", "Duplicate-path removal started.")
}

func (s *Server) promoteAliasPathPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	pathIDRaw := r.FormValue("path_id")
	pathID, err := strconv.ParseInt(pathIDRaw, 10, 64)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, "Invalid path id.")
		return
	}
	var imageID int64
	var newPath string
	var alreadyCanonical int
	if err := s.db().Read.QueryRow(
		`SELECT image_id, path, is_canonical FROM image_paths WHERE id = ?`, pathID,
	).Scan(&imageID, &newPath, &alreadyCanonical); err != nil {
		flashStatus(w, http.StatusNotFound, "Path not found.")
		return
	}
	if alreadyCanonical == 1 {
		writeInlineFlash(w, "ok", "Already canonical.")
		return
	}
	if _, statErr := os.Stat(newPath); statErr != nil {
		flashStatus(w, http.StatusBadRequest, "Cannot promote: file is missing on disk.")
		return
	}
	if err := gallery.PromoteCanonicalByPathID(s.db(), s.boundary(), imageID, pathID, newPath); err != nil {
		code := http.StatusInternalServerError
		if errors.As(err, new(gallery.Exclusion)) {
			code = http.StatusBadRequest
		}
		flashStatus(w, code, err.Error())
		return
	}
	s.active().InvalidateCaches()
	writeInlineFlash(w, "ok", "Promoted to canonical.")
}

func (s *Server) rebuildThumbnailsPost(w http.ResponseWriter, r *http.Request) {
	if err := s.startRebuildThumbsJob(s.active()); err != nil {
		if errors.Is(err, jobs.ErrJobRunning) {
			flashStatus(w, http.StatusConflict, "A job is already running.")
			return
		}
		writeInlineFlash(w, "err", err.Error())
		return
	}
	writeInlineFlash(w, "ok", "Thumbnail rebuild started.")
}

func (s *Server) startRebuildThumbsJob(cx *galleryCtx) error {
	if cx == nil || cx.DB == nil {
		return fmt.Errorf("no gallery context")
	}
	type imgRow struct {
		ID       int64
		Path     string
		FileType string
		Width    sql.NullInt64
	}
	imgs, err := db.QueryAll(cx.DB.Read, func(rows *sql.Rows) (imgRow, error) {
		var img imgRow
		err := rows.Scan(&img.ID, &img.Path, &img.FileType, &img.Width)
		return img, err
	}, `SELECT id, canonical_path, file_type, width FROM images WHERE is_missing = 0`)
	if err != nil {
		return err
	}

	if err := s.jobs.Start(models.JobTypeRebuildThumbs); err != nil {
		return err
	}
	thumbnailsPath := cx.ThumbnailsPath
	galleryName := cx.Name
	database := cx.DB
	go func() {
		ctx := s.jobs.Context()
		processed, rebuilt := 0, 0
		total := len(imgs)
		failedNote := func() string {
			if processed == rebuilt {
				return ""
			}
			return fmt.Sprintf(", %d failed", processed-rebuilt)
		}
		for _, img := range imgs {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("[%s] rebuild cancelled (%d/%d rebuilt%s)", galleryName, rebuilt, total, failedNote()))
				return
			}
			s.jobs.Update(processed, total, fmt.Sprintf("[%s] rebuilding…", galleryName))
			if err := gallery.Rebuild(img.Path, thumbnailsPath, img.ID, img.FileType); err != nil {
				logx.Warnf("rebuild thumbnail for %d: %v", img.ID, err)
			} else {
				rebuilt++
			}
			// An AVIF or JPEG XL decodes to answer, so it is probed only
			// when its size is missing.
			if gallery.IsVideoType(img.FileType) || (gallery.IsFFmpegStill(img.FileType) && !img.Width.Valid) {
				if w, h, ok := gallery.ProbeVideoDimensions(img.Path); ok {
					if _, err := database.Write.ExecContext(ctx,
						`UPDATE images SET width = ?, height = ? WHERE id = ?`,
						w, h, img.ID,
					); err != nil {
						logx.Warnf("backfill video dimensions for %d: %v", img.ID, err)
					}
				}
			}
			processed++
		}
		s.jobs.Complete(fmt.Sprintf("[%s] rebuilt %d thumbnail(s)%s.", galleryName, rebuilt, failedNote()))
	}()
	return nil
}

// phash first: it reads small thumbnails, so a cancel during the md5 pass
// over every original still leaves it done.
func (s *Server) computeHashesPost(w http.ResponseWriter, r *http.Request) {
	if !s.startJob(w, models.JobTypeHashes) {
		return
	}
	database := s.db()
	thumbnailsPath := s.thumbnailsPath()
	active := s.active()
	tree := active.BKTree
	go func() {
		ctx := s.jobs.Context()
		phashed, phashUpdated, err := relations.BackfillPhashes(ctx, database, thumbnailsPath, func(p, total int, _ string) {
			s.jobs.Update(p, total, "Perceptual hashes…")
		})
		// A rebuild on next use beats thousands of incremental Inserts.
		// Reset before the md5 pass, so cancelling that still leaves the
		// tree in step.
		if tree != nil {
			tree.Reset()
		}
		active.InvalidatePhashMissing()
		cancelled := func(err error) bool { return err == context.Canceled || ctx.Err() != nil }
		if cancelled(err) {
			s.jobs.Complete(fmt.Sprintf("hashes cancelled (%d phash, 0 md5)", phashUpdated))
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		summed, sumUpdated, err := gallery.BackfillMD5s(ctx, database, func(p, total int, _ string) {
			s.jobs.Update(p, total, "MD5…")
		})
		if cancelled(err) {
			s.jobs.Complete(fmt.Sprintf("hashes cancelled (%d phash, %d md5)", phashUpdated, sumUpdated))
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(fmt.Sprintf("Computed %d perceptual hash(es) of %d and %d md5 digest(s) of %d.",
			phashUpdated, phashed, sumUpdated, summed))
	}()
	writeInlineFlash(w, "ok", "Hash backfill started.")
}

func (s *Server) generateMetaTagsPost(w http.ResponseWriter, r *http.Request) {
	if !s.startJob(w, models.JobTypeMetaTags) {
		return
	}
	database := s.db()
	thumbnailsPath := s.thumbnailsPath()
	active := s.active()
	go func() {
		ctx := s.jobs.Context()
		processed, updated, err := gallery.BackfillMetaTags(ctx, database, thumbnailsPath, func(p, total int, msg string) {
			s.jobs.Update(p, total, cmp.Or(msg, "Meta tags…"))
		})
		active.InvalidateCaches()
		if err != nil {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("meta tags cancelled (%d processed, %d updated)", processed, updated))
				return
			}
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(fmt.Sprintf("Derived meta tags for %d image(s); %d changed.", processed, updated))
	}()
	writeInlineFlash(w, "ok", "Meta tag pass started.")
}

func (s *Server) indexWorkflowsPost(w http.ResponseWriter, r *http.Request) {
	if !s.startJob(w, models.JobTypeIndexWork) {
		return
	}
	database := s.db()
	active := s.active()
	go func() {
		ctx := s.jobs.Context()
		if _, err := database.Write.ExecContext(ctx, `UPDATE comfyui_metadata SET terms_version = 0`); err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		indexed, err := gallery.BackfillComfyTerms(ctx, database, func(p, total int, msg string) {
			s.jobs.Update(p, total, cmp.Or(msg, "Workflows…"))
		})
		active.InvalidateCaches()
		if err != nil {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("workflow index cancelled (%d indexed)", indexed))
				return
			}
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(fmt.Sprintf("Indexed %d ComfyUI workflow(s).", indexed))
	}()
	writeInlineFlash(w, "ok", "Workflow indexing started.")
}

func (s *Server) removeMetaTagsPost(w http.ResponseWriter, r *http.Request) {
	if !s.startJob(w, models.JobTypeMetaTags) {
		return
	}
	database := s.db()
	active := s.active()
	go func() {
		ctx := s.jobs.Context()
		processed, removed, err := gallery.RemoveMetaTags(ctx, database, func(p, total int, msg string) {
			s.jobs.Update(p, total, cmp.Or(msg, "Meta tags…"))
		})
		active.InvalidateCaches()
		if err != nil {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("removal cancelled (%d image(s), %d tag(s) removed)", processed, removed))
				return
			}
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(fmt.Sprintf("Removed %d generated tag(s) from %d image(s).", removed, processed))
	}()
	writeInlineFlash(w, "ok", "Removing generated meta tags.")
}

func (s *Server) vacuumDBPost(w http.ResponseWriter, r *http.Request) {
	if !s.startJob(w, models.JobTypeVacuum) {
		return
	}
	go func() {
		beforeSize := dbFileSize(s.dbPath())
		if _, err := s.db().Write.Exec(`VACUUM`); err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		// In WAL mode VACUUM writes into the -wal file; only a truncating
		// checkpoint gives the space back.
		if _, err := s.db().Write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			logx.Warnf("vacuum wal_checkpoint: %v", err)
		}
		afterSize := dbFileSize(s.dbPath())
		freed := max(beforeSize-afterSize, 0)
		s.jobs.Complete(fmt.Sprintf("Vacuumed (reclaimed %s).", humanBytesFmt(freed)))
	}()
	writeInlineFlash(w, "ok", "Vacuum started. Watch the status bar for the reclaimed-space report.")
}

// Holds a job slot so an autotag cannot start before ReleaseAll and lose
// its worker mid-inference.
func (s *Server) freeMemoryPost(w http.ResponseWriter, r *http.Request) {
	if err := s.jobs.Start(models.JobTypeFreeMemory); err != nil {
		flashStatus(w, http.StatusConflict, "A job is running; try again when it finishes.")
		return
	}
	defer s.jobs.Complete("Memory caches released.")
	before := readVmRSS()
	ctxs := s.allContexts()
	for _, cx := range ctxs {
		if err := shrinkGalleryMemory(cx); err != nil {
			logx.Warnf("free memory: shrink %q: %v", cx.Name, err)
		}
	}
	debug.FreeOSMemory()
	tagger.ReleaseAll()
	after := readVmRSS()
	if before > 0 && after > 0 && before > after {
		writeInlineFlash(w, "ok", "Freed "+humanBytesFmt(int64(before-after))+".")
		return
	}
	writeInlineFlash(w, "ok", "Memory caches released.")
}

// Counts the WAL and shm too: after a mass delete the WAL can hold most
// of the pages.
func dbFileSize(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

func humanBytesFmt(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func (s *Server) reExtractMetadataPost(w http.ResponseWriter, r *http.Request) {
	// Read into a slice so the cursor closes before the job starts,
	// rather than holding a read connection for the whole run.
	type imgRow struct {
		ID        int64
		Path      string
		FileType  string
		sdHash    string
		comfyHash string
		source    string
	}

	imgs, err := db.QueryAll(s.db().Read, func(rows *sql.Rows) (imgRow, error) {
		var img imgRow
		err := rows.Scan(&img.ID, &img.Path, &img.FileType, &img.source, &img.sdHash, &img.comfyHash)
		return img, err
	}, `
		SELECT i.id, i.canonical_path, i.file_type, i.source_type,
		       COALESCE(sm.generation_hash, ''),
		       COALESCE(cm.generation_hash, '')
		FROM images i
		LEFT JOIN sd_metadata sm ON sm.image_id = i.id
		LEFT JOIN comfyui_metadata cm ON cm.image_id = i.id
		WHERE i.is_missing = 0
	`)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}

	if !s.startJob(w, models.JobTypeReExtract) {
		return
	}

	database := s.db()
	thumbnailsPath := s.thumbnailsPath()
	active := s.active()
	go func() {
		ctx := s.jobs.Context()
		processed := 0
		updated := 0
		total := len(imgs)
		for _, img := range imgs {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("re-extraction cancelled (%d/%d processed, %d updated)", processed, total, updated))
				return
			}
			s.jobs.Update(processed, total, "Processing…")
			if h, err := gallery.RecomputeAndStorePhash(ctx, database, img.ID, thumbnailsPath); err == nil {
				relations.PhashStored(database, img.ID, h)
			} else {
				logx.Debugf("re-extract phash %d: %v", img.ID, err)
			}
			sdMeta, comfyMeta, _ := meta.Extract(img.Path, img.FileType)

			sourceType := models.SourceTypeNone
			if sdMeta != nil && comfyMeta != nil {
				sourceType = models.SourceTypeBoth
			} else if sdMeta != nil {
				sourceType = models.SourceTypeA1111
			} else if comfyMeta != nil {
				sourceType = models.SourceTypeComfyUI
			}

			newSDHash := ""
			if sdMeta != nil {
				newSDHash = sdMeta.GenerationHash
			}
			newComfyHash := ""
			if comfyMeta != nil {
				newComfyHash = comfyMeta.GenerationHash
			}

			var durationSec *float64
			if gallery.IsVideoType(img.FileType) {
				if d, ok := gallery.ProbeDurationSeconds(img.Path); ok {
					durationSec = &d
				}
			}

			// Unchanged hashes skip the rewrite; a probed duration always
			// rewrites, since the old one is not read to compare.
			if newSDHash == img.sdHash && newComfyHash == img.comfyHash && sourceType == img.source && durationSec == nil {
				processed++
				continue
			}

			if err := reExtractApply(ctx, database, img.ID, sourceType, durationSec, sdMeta, comfyMeta); err != nil {
				logx.Warnf("re-extract image %d: %v", img.ID, err)
				processed++
				continue
			}
			processed++
			updated++
			// source_type moved, so the ai-generated tag has to follow it.
			if err := gallery.ApplyMetaTags(database, thumbnailsPath, img.ID); err != nil {
				logx.Debugf("re-extract meta tags %d: %v", img.ID, err)
			}
		}
		active.InvalidatePhashMissing()
		active.InvalidateCaches()
		s.jobs.Complete(fmt.Sprintf("Re-extracted metadata for %d image(s) (%d updated).", processed, updated))
	}()

	writeInlineFlash(w, "ok", "Re-extraction started.")
}

// One transaction, so a failure never leaves the new source_type over
// missing metadata. A nil durationSec leaves the column alone.
func reExtractApply(ctx context.Context, database *db.DB, imageID int64, sourceType string, durationSec *float64, sdMeta *models.SDMetadata, comfyMeta *models.ComfyUIMetadata) error {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE images SET source_type = ? WHERE id = ?`, sourceType, imageID); err != nil {
		return fmt.Errorf("update source_type: %w", err)
	}
	if durationSec != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE images SET duration_seconds = ? WHERE id = ?`, *durationSec, imageID); err != nil {
			return fmt.Errorf("update duration_seconds: %w", err)
		}
	}
	if err := gallery.ReplaceGenerationMetadata(ctx, tx, imageID, sdMeta, comfyMeta); err != nil {
		return err
	}
	return tx.Commit()
}
