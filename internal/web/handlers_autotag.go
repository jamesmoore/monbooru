package web

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tagger"
)

// The whole scope and its per-image tagger state sit in memory.
const autotagSearchScopeCap = 50000

var errAutotagOverCap = errors.New("autotag: search-scope cap reached")

func (s *Server) spawnAutoTagJob(ids []int64, selected []tagger.TaggerStatus, logScope, itemNoun string) {
	cfg := s.cfgSnapshot()
	database := s.db()
	cx := s.active()
	baseline := readVmRSS()
	go func() {
		ctx := s.jobs.Context()
		skipped, err := tagger.RunWithTaggers(ctx, database, cfg, ids, selected, s.jobs, cfg.Tagger.ExecutionProvider, cx.MangaCacheDir())
		_ = s.completeAutotagRun(cx, ctx, "", itemNoun, logScope, len(ids), skipped, baseline, err)
	}()
}

func (s *Server) completeAutotagRun(cx *galleryCtx, ctx context.Context, prefix, itemNoun, logScope string, total, skipped int, baseline uint64, err error) error {
	// Unconditional: a cancelled or failed run still wrote rows for the
	// images it finished.
	cx.InvalidateCaches()
	if ctx.Err() != nil {
		s.jobs.Complete(fmt.Sprintf("%sauto-tagging cancelled (%d image(s) queued)", prefix, total))
		return nil
	}
	if err != nil {
		s.jobs.Fail(err.Error())
		return err
	}
	logAutotagPeak(fmt.Sprintf("%s %d image(s)", logScope, total), baseline)
	if skipped > 0 {
		s.jobs.Complete(fmt.Sprintf("%sauto-tagged %d of %d %simage(s), %d skipped", prefix, total-skipped, total, itemNoun, skipped))
		return nil
	}
	s.jobs.Complete(fmt.Sprintf("%sauto-tagged %d %simage(s)", prefix, total, itemNoun))
	return nil
}

func logAutotagPeak(scope string, baselineRSS uint64) {
	if baselineRSS == 0 {
		return
	}
	peak := readVmHWM()
	if peak <= baselineRSS {
		return
	}
	logx.Infof("autotag %s: peak RSS +%s", scope, humanBytesFmt(int64(peak-baselineRSS)))
}

func (s *Server) uploadPost(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgSnapshot()
	maxFileSizeMB := cfg.Gallery.MaxFileSizeMB
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024
	if maxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes*10+4096) // ten files at the cap, plus the other fields
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeInlineFlash(w, "err", "Upload too large or invalid.")
		return
	}
	// Taken only once the body is in: a slow upload holding it would stall
	// every request queued behind a gallery switch.
	s.ctxMu.RLock()
	defer s.ctxMu.RUnlock()
	if pageGalleryStale(w, r, s.activeGallery()) {
		return
	}
	if cx := s.active(); cx == nil || cx.Degraded {
		flashStatus(w, http.StatusServiceUnavailable, "Upload unavailable: gallery path is unreadable.")
		return
	}

	tagInput := strings.TrimSpace(r.FormValue("tags"))
	autotagAfter := r.FormValue("autotag") == "on"
	folderInput, naming := gallery.ReceivedNaming(s.activeGallery(),
		strings.TrimSpace(r.FormValue("folder")),
		strings.TrimSpace(cfg.Gallery.DefaultUploadFolder),
		strings.TrimSpace(cfg.Gallery.DefaultUploadName))
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeInlineFlash(w, "err", "No files selected.")
		return
	}

	destDir, destErr := s.boundary().ResolveSubdir(folderInput)
	if destErr != nil {
		writeInlineFlash(w, "err", destErr.Error())
		return
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		writeInlineFlash(w, "err", "Could not create folder: "+err.Error())
		return
	}

	var tagPairs []catTag
	if tagInput != "" {
		tagPairs, _, _ = s.parseTagInput(tagInput)
	}

	var totalBytes int64
	for _, fh := range files {
		totalBytes += fh.Size
	}
	logx.Infof("upload: ingesting %d file(s) (%s)", len(files), humanBytesFmt(totalBytes))

	var addedIDs []int64
	var dupeIDs []int64
	var tagWarnings []string
	var refused, tooBig []string
	added, dupes, oversized := 0, 0, 0
	unsupported, ignored, unsaved, noPreview := 0, 0, 0, 0
	for _, fh := range files {
		// The body limit only bounds the total, so one file can still be
		// over the cap.
		if maxBytes > 0 && fh.Size > maxBytes {
			oversized++
			tooBig = append(tooBig, fh.Filename)
			continue
		}
		file, err := fh.Open()
		if err != nil {
			unsaved++
			refused = append(refused, fh.Filename)
			continue
		}

		dstPath := gallery.UniqueDestPath(destDir, fh.Filename)
		if s.boundary().Check(dstPath) != nil {
			_ = file.Close()
			ignored++
			refused = append(refused, fh.Filename)
			continue
		}
		dst, err := os.Create(dstPath)
		if err != nil {
			_ = file.Close()
			unsaved++
			refused = append(refused, fh.Filename)
			continue
		}

		if _, err := dst.ReadFrom(file); err != nil {
			_ = dst.Close()
			_ = file.Close()
			_ = os.Remove(dstPath)
			unsaved++
			refused = append(refused, fh.Filename)
			continue
		}
		_ = dst.Close()
		_ = file.Close()

		if _, ftErr := gallery.DetectFileType(dstPath); ftErr != nil {
			_ = os.Remove(dstPath)
			unsupported++
			refused = append(refused, fh.Filename)
			continue
		}

		img, isDup, ingestErr := gallery.Ingest(s.db(), s.galleryPath(), s.thumbnailsPath(), dstPath, models.OriginUpload)
		if ingestErr != nil {
			logx.Warnf("upload ingest %q: %v", fh.Filename, ingestErr)
			// Only bytes the ingest can never accept are dropped; a
			// transient failure keeps the operator's file.
			if errors.Is(ingestErr, gallery.ErrUnsupportedType) {
				_ = os.Remove(dstPath)
				unsupported++
			} else {
				unsaved++
			}
			refused = append(refused, fh.Filename)
			continue
		}
		if isDup {
			gallery.DropDuplicateCopy(s.db(), img.ID, dstPath, "upload")
			dupeIDs = append(dupeIDs, img.ID)
			dupes++
			continue
		}

		if _, err := naming.Apply(r.Context(), s.db(), s.boundary(), img.ID, "", ""); err != nil {
			logx.Warnf("upload: name %d: %v", img.ID, err)
		}

		for _, ct := range tagPairs {
			tag, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
			if err != nil {
				tagWarnings = append(tagWarnings, ct.name+": "+err.Error())
				continue
			}
			if err := s.tagSvc().AddTagToImage(img.ID, tag.ID, false, nil); err != nil {
				tagWarnings = append(tagWarnings, ct.name+": "+err.Error())
			}
		}
		// Ingest only logs a failed thumbnail, so the missing file is the
		// one signal.
		if _, statErr := os.Stat(gallery.ThumbnailPath(s.thumbnailsPath(), img.ID)); statErr != nil {
			noPreview++
		}
		addedIDs = append(addedIDs, img.ID)
		added++
	}

	if added > 0 {
		batch := time.Now().UnixNano()
		if err := db.Chunked(addedIDs, 500, func(chunk []int64) error {
			placeholders, args := db.InPlaceholders(chunk)
			_, execErr := s.db().Write.Exec(
				`UPDATE images SET upload_batch = ? WHERE id IN (`+placeholders+`)`,
				append([]any{batch}, args...)...)
			return execErr
		}); err != nil {
			logx.Warnf("upload: stamp batch token: %v", err)
		}
		s.active().InvalidateCaches()
	}

	// Assembled as HTML for the duplicate links: numbers are safe, and
	// every operator or file supplied string must be escaped.
	var msg strings.Builder
	fmt.Fprintf(&msg, "%d added", added)
	if dupes > 0 {
		fmt.Fprintf(&msg, ", %d duplicate(s)", dupes)
		if len(dupeIDs) > 0 {
			msg.WriteString(" (")
			for i, id := range dupeIDs {
				if i > 0 {
					msg.WriteString(", ")
				}
				fmt.Fprintf(&msg, `<a href="/images/%d">#%d</a>`, id, id)
			}
			msg.WriteString(")")
		}
	}
	if noPreview > 0 {
		fmt.Fprintf(&msg, ", %d without a preview", noPreview)
	}
	if oversized > 0 {
		fmt.Fprintf(&msg, ", %d skipped over %d MB%s", oversized, maxFileSizeMB, namedFiles(tooBig))
	}
	failed := unsupported + ignored + unsaved
	if failed > 0 {
		fmt.Fprintf(&msg, ", %d error(s): %s", failed, uploadErrorReasons(unsupported, ignored, unsaved, refused))
	}
	if len(tagWarnings) > 0 {
		fmt.Fprintf(&msg, " (%d tag warning(s): %s)", len(tagWarnings), html.EscapeString(strings.Join(tagWarnings, "; ")))
	}
	kind := "ok"
	switch {
	case added == 0 && (failed > 0 || oversized > 0):
		kind = "err"
	case failed > 0 || oversized > 0:
		kind = "warn"
	}

	if autotagAfter && len(addedIDs) > 0 && tagger.IsAvailable(cfg) {
		selected, selErr := tagger.SelectForGallery(cfg, s.activeGallery(), taggerName)
		if selErr != nil {
			fmt.Fprintf(&msg, " (autotag skipped: %s)", html.EscapeString(selErr.Error()))
		} else if err := s.jobs.Start(models.JobTypeAutotag); err != nil {
			msg.WriteString(" (autotag skipped: a job is already running)")
		} else {
			s.spawnAutoTagJob(addedIDs, selected, "upload", "uploaded ")
			fmt.Fprintf(&msg, ", auto-tagging %d image(s)", len(addedIDs))
		}
	}
	writeInlineFlashHTML(w, kind, msg.String())
	_, _ = w.Write([]byte(s.inboxNavOOB(r)))
}

const maxNamedRefusals = 3

func namedFiles(names []string) string {
	if len(names) == 0 || len(names) > maxNamedRefusals {
		return ""
	}
	return " (" + html.EscapeString(strings.Join(names, ", ")) + ")"
}

func uploadErrorReasons(unsupported, ignored, unsaved int, refused []string) string {
	reason := "could not be saved"
	var counted []string
	for _, r := range []struct {
		n             int
		bare, counted string
	}{
		{unsupported, "unsupported file type", "%d unsupported file type(s)"},
		{ignored, "on the ignore list", "%d on the ignore list"},
		{unsaved, "could not be saved", "%d could not be saved"},
	} {
		if r.n > 0 {
			reason = r.bare
			counted = append(counted, fmt.Sprintf(r.counted, r.n))
		}
	}
	if len(counted) > 1 {
		reason = strings.Join(counted, ", ")
	}
	return reason + namedFiles(refused)
}

func (s *Server) autotagTrigger(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgSnapshot()
	if !tagger.IsAvailable(cfg) {
		http.Error(w, "auto-tagger not available: "+tagger.UnavailableReason(cfg), http.StatusServiceUnavailable)
		return
	}

	if !parseFormOK(w, r) {
		return
	}
	scope := strings.TrimSpace(r.FormValue("scope"))
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))

	selected, selErr := tagger.SelectForGallery(cfg, s.activeGallery(), taggerName)
	if selErr != nil {
		externalErr(w, r, selErr.Error(), http.StatusBadRequest)
		return
	}

	var ids []int64
	if scope == "search" {
		expr := search.Parse(r.FormValue("q"))
		expr = resolveCeiling(r, s.active()).Apply(expr)
		err := search.Scope{Expr: expr}.Stream(s.db(), func(t search.DeleteTarget) error {
			if len(ids) >= autotagSearchScopeCap {
				return errAutotagOverCap
			}
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil && err != errAutotagOverCap {
			logx.Errorf("autotag search: %v", err)
			hxErr(w, r, "Search error.", "search error", http.StatusInternalServerError)
			return
		}
		if err == errAutotagOverCap {
			msg := fmt.Sprintf("Search matches more than %d images; narrow the query and re-run.", autotagSearchScopeCap)
			externalErr(w, r, msg, http.StatusBadRequest)
			return
		}
	} else {
		ids = parseIDList(r.Form["ids"])
	}

	if len(ids) == 0 {
		hxErr(w, r, "No images to tag.", "no images selected", http.StatusBadRequest)
		return
	}

	if err := s.jobs.Start(models.JobTypeAutotag); err != nil {
		hxErr(w, r, "A job is already running.", "job already running", http.StatusConflict)
		return
	}
	s.jobs.Update(0, len(ids), "starting (loading model may take a few seconds)…")

	s.spawnAutoTagJob(ids, selected, "batch", "")

	if isHTMXRequest(r) {
		setFlashHeader(w, fmt.Sprintf("Auto-tagger started for %d image(s).", len(ids)), "ok", nil)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) autotagImage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgSnapshot()
	if !tagger.IsAvailable(cfg) {
		reason := tagger.UnavailableReason(cfg)
		hxErr(w, r, "Auto-tagger not available: "+reason+".", "auto-tagger not available: "+reason, http.StatusServiceUnavailable)
		return
	}

	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))

	selected, selErr := tagger.SelectForGallery(cfg, s.activeGallery(), taggerName)
	if selErr != nil {
		externalErr(w, r, selErr.Error(), http.StatusBadRequest)
		return
	}

	if err := s.jobs.Start(models.JobTypeAutotag); err != nil {
		hxErr(w, r, "A job is already running.", "job already running", http.StatusConflict)
		return
	}
	s.jobs.Update(0, 1, "starting (loading model may take a few seconds)…")

	database := s.db()
	cx := s.active()
	baseline := readVmRSS()
	go func() {
		// CPU for a single image: starting a GPU session takes longer
		// than CPU takes to tag it.
		ctx := s.jobs.Context()
		skipped, err := tagger.RunWithTaggers(ctx, database, cfg, []int64{id}, selected, s.jobs, "cpu", cx.MangaCacheDir())
		cx.InvalidateCaches()
		if ctx.Err() != nil {
			s.jobs.Complete("auto-tagging cancelled")
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		logAutotagPeak(fmt.Sprintf("image #%d", id), baseline)
		if skipped > 0 {
			s.jobs.Complete(fmt.Sprintf("auto-tagger skipped image #%d", id))
			return
		}
		s.jobs.Complete(fmt.Sprintf("auto-tagged image #%d", id))
	}()

	if isHTMXRequest(r) {
		setFlashHeader(w, "Auto-tagger started for this image.", "ok", nil)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", id), http.StatusSeeOther)
}
