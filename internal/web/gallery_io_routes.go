package web

import (
	"cmp"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
)

func (s *Server) importGallery(name, format string, upload io.Reader) (int, error) {
	newCx, leftOut, err := s.replaceGalleryFromUpload(name, format, upload)
	if err != nil {
		return leftOut, err
	}
	logx.Infof("gallery: imported %q (format=%s)", name, format)

	// Before the rebuild starts: switchGallery refuses while a job runs.
	if err := s.switchGallery(name); err != nil {
		logx.Infof("gallery %q: post-import switch skipped: %v", name, err)
	}

	// The import wiped the thumbnails directory.
	if err := s.startRebuildThumbsJob(newCx); err != nil {
		logx.Infof("gallery %q: skipped post-import rebuild: %v", name, err)
	}
	return leftOut, nil
}

func leftOutNote(leftOut int) string {
	if leftOut == 0 {
		return ""
	}
	return fmt.Sprintf(" %d file(s) in the archive sit in folders this gallery leaves out and were not extracted.", leftOut)
}

// Holds the job lane, not ctxMu, for these minutes of work: a held ctxMu
// freezes every request, and every mutation and job already refuses while
// the lane is held.
func (s *Server) replaceGalleryFromUpload(name, format string, upload io.Reader) (*galleryCtx, int, error) {
	if err := s.jobs.BeginSchedule(); err != nil {
		return nil, 0, errJobRunning
	}
	defer s.jobs.EndSchedule()

	s.ctxMu.Lock()
	st := s.galleryState()
	cx, ok := st.contexts[name]
	if !ok {
		s.ctxMu.Unlock()
		return nil, 0, fmt.Errorf("unknown gallery %q", name)
	}
	if name == st.active {
		s.ctxMu.Unlock()
		return nil, 0, fmt.Errorf("cannot import over the active gallery; switch to another first")
	}
	s.cfgMu.RLock()
	isDefault := name == s.cfg.DefaultGallery
	s.cfgMu.RUnlock()
	if isDefault {
		s.ctxMu.Unlock()
		return nil, 0, fmt.Errorf("cannot import over the default gallery; set another as default first")
	}
	galleryPath := cx.GalleryPath
	bound := cx.Boundary()
	dbPath := cx.DBPath
	thumbsPath := cx.ThumbnailsPath
	dataDir := filepath.Dir(dbPath)
	s.ctxMu.Unlock()

	// Next to the database so the rename into place stays on one filesystem,
	// and before the close so a failed upload leaves the gallery open.
	tmp, err := os.CreateTemp(dataDir, "import-*.upload")
	if err != nil {
		return nil, 0, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := io.Copy(tmp, upload); err != nil {
		_ = tmp.Close()
		return nil, 0, fmt.Errorf("buffer upload: %w", err)
	}
	_ = tmp.Close()

	s.ctxMu.Lock()
	if err := exportRunning(cx); err != nil {
		s.ctxMu.Unlock()
		return nil, 0, err
	}
	cx.Close()
	s.ctxMu.Unlock()

	leftOut, applyErr := galleryio.ApplyImport(format, tmpPath, dbPath, thumbsPath, bound, s.maxFileSizeMB())

	// Reopened even after a failed import, so the gallery stays usable.
	newCx, openErr := library.Open(config.Gallery{
		Name: name, GalleryPath: galleryPath, DBPath: dbPath, ThumbnailsPath: thumbsPath,
	})
	if openErr != nil {
		if applyErr != nil {
			return nil, leftOut, fmt.Errorf("import failed: %w (reopen also failed: %v)", applyErr, openErr)
		}
		return nil, leftOut, fmt.Errorf("reopen gallery: %w", openErr)
	}
	s.ctxMu.Lock()
	next := s.galleryState().clone()
	next.contexts[name] = newCx
	s.galState.Store(next)
	s.rebuildBoundaries()
	watch, maxMB := s.watcherSettings()
	newCx.StartBackground(watch, maxMB, s.ingestNaming(newCx.Name), s.jobs)
	s.ctxMu.Unlock()

	go newCx.WarmCaches()
	return newCx, leftOut, applyErr
}

func (s *Server) settingsGalleryExport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	withImages := r.URL.Query().Get("with_images") == "true"

	if s.get(name) == nil {
		http.Error(w, "unknown gallery", http.StatusNotFound)
		return
	}
	switch format {
	case "db", "json", "light":
	default:
		http.Error(w, "format must be db, json, or light", http.StatusBadRequest)
		return
	}

	filename, contentType := exportFilename(name, format, withImages)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	var err error
	switch {
	case format == "light" && withImages:
		err = s.exportGalleryLight(name, w)
	case format == "light":
		err = s.exportGalleryLightManifest(name, w)
	case withImages:
		err = s.exportGalleryArchive(name, format, w)
	case format == "db":
		err = s.exportGalleryDB(name, w)
	case format == "json":
		err = s.exportGalleryJSON(name, w)
	}
	if err != nil {
		logx.Warnf("gallery export %q: %v", name, err)
		// The headers are out: a clean end would pass a truncated file
		// off as whole.
		panic(http.ErrAbortHandler)
	}
}

func exportFilename(name, format string, withImages bool) (string, string) {
	if format == "light" && withImages {
		return name + "-light.zip", "application/zip"
	}
	if format == "light" {
		return name + "-light.json", "application/json"
	}
	if withImages {
		return name + ".zip", "application/zip"
	}
	switch format {
	case "db":
		return name + ".db", "application/vnd.sqlite3"
	case "json":
		return name + ".json", "application/json"
	}
	return name, "application/octet-stream"
}

// The file part comes back unread, so an upload streams instead of spooling
// to a temp file.
func readFieldsToFile(mr *multipart.Reader) (map[string]string, *multipart.Part, error) {
	const maxFieldBytes = 1 << 20
	fields := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return fields, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if part.FileName() != "" {
			return fields, part, nil
		}
		body, err := io.ReadAll(io.LimitReader(part, maxFieldBytes))
		_ = part.Close()
		if err != nil {
			return nil, nil, err
		}
		fields[part.FormName()] = strings.TrimSpace(string(body))
	}
}

// Parts are read in order so the name check runs before the file is
// consumed: the form must put mode and confirm_name ahead of the file.
func (s *Server) settingsGalleryImport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	const maxImport = 16 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxImport)

	mr, err := r.MultipartReader()
	if err != nil {
		writeInlineFlash(w, "err", "expected multipart/form-data")
		return
	}
	fields, filePart, err := readFieldsToFile(mr)
	if err != nil {
		writeInlineFlash(w, "err", "malformed upload")
		return
	}
	if filePart == nil {
		writeInlineFlash(w, "err", "missing file")
		return
	}
	defer func() { _ = filePart.Close() }()
	fileFilename := filePart.FileName()

	mode := fields["mode"]
	mode = cmp.Or(mode, "replace")
	if mode != "replace" && mode != "merge" {
		writeInlineFlash(w, "err", "mode must be replace or merge")
		return
	}
	if mode == "replace" {
		if fields["confirm_name"] != name {
			writeInlineFlash(w, "err", "type-to-confirm name does not match")
			return
		}
	}
	format := galleryio.FormatFromExt(fileFilename)
	if format == "" {
		writeInlineFlash(w, "err", "file must be .db, .json, or .zip")
		return
	}

	if mode == "merge" {
		res, err := s.mergeGallery(name, format, filePart)
		if err != nil {
			writeInlineFlash(w, "err", err.Error())
			return
		}
		if err := s.switchGallery(name); err != nil {
			logx.Infof("gallery %q: post-merge switch skipped: %v", name, err)
		}
		writeInlineFlash(w, "ok", "Gallery "+name+" merged: "+res.Summary()+".")
		return
	}
	leftOut, err := s.importGallery(name, format, filePart)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	writeInlineFlash(w, "ok", "Gallery "+name+" imported. Rebuilding thumbnails in the background."+leftOutNote(leftOut))
}

func (s *Server) batchTransfer(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	dstCx, removeAfter, ok := s.transferTarget(w, r)
	if !ok {
		return
	}
	s.startScopedJob(w, r, "batch-transfer", models.JobTypeTransfer, false, func(ids []int64) {
		s.runBatchTransfer(ids, dstCx, removeAfter)
	}, dstCx.Name)
}

func (s *Server) transferImage(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	dstCx, removeAfter, ok := s.transferTarget(w, r)
	if !ok {
		return
	}
	if !s.startJob(w, models.JobTypeTransfer, dstCx.Name) {
		return
	}
	srcCx := s.active()
	if err := s.transferOneImage(srcCx, dstCx, id, removeAfter); err != nil {
		s.jobs.Fail(err.Error())
		flashStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	dstCx.InvalidateCaches()
	if removeAfter {
		srcCx.InvalidateCaches()
	}
	msg := fmt.Sprintf("Transferred image to %s.", dstCx.Name)
	s.jobs.Complete(msg)

	dest := fmt.Sprintf("/images/%d", id)
	if removeAfter {
		dest = "/"
	}
	if isHTMXRequest(r) {
		setFlashHeader(w, msg, "ok", nil)
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) transferTarget(w http.ResponseWriter, r *http.Request) (*galleryCtx, bool, bool) {
	target := strings.TrimSpace(r.FormValue("target"))
	msg := ""
	switch target {
	case "":
		msg = "Pick a target gallery."
	case s.activeGallery():
		msg = "The target must be a different gallery."
	}
	if msg == "" {
		if dst := s.get(target); dst == nil || dst.DB == nil {
			msg = "Unknown target gallery."
		} else if dst.Degraded {
			msg = "The target gallery is unavailable."
		} else {
			return dst, r.FormValue("remove_after") != "", true
		}
	}
	flashStatus(w, http.StatusBadRequest, msg)
	return nil, false, false
}

func (s *Server) runBatchTransfer(ids []int64, dstCx *galleryCtx, removeAfter bool) {
	srcCx := s.active()
	total := len(ids)
	transferred, failed, cancelled := s.perImageLoop(ids, "transfer", "transferring", func(_ int, id int64) error {
		return s.transferOneImage(srcCx, dstCx, id, removeAfter)
	})

	if transferred > 0 {
		dstCx.InvalidateCaches()
		if removeAfter {
			srcCx.InvalidateCaches()
		}
	}
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("transfer cancelled (%d/%d done)", transferred, total))
		return
	}
	summary := fmt.Sprintf("Transferred %d image(s) to %s.", transferred, dstCx.Name)
	if failed > 0 {
		summary = fmt.Sprintf("Transferred %d image(s) to %s, %d failed.", transferred, dstCx.Name, failed)
	}
	s.jobs.Complete(summary)
}

func (s *Server) exportGalleryDB(name string, w io.Writer) error {
	return s.exportGallery(name, func(cx *galleryCtx) error { return galleryio.ExportGalleryDB(cx.Handle, w) })
}

func (s *Server) exportGalleryJSON(name string, w io.Writer) error {
	return s.exportGallery(name, func(cx *galleryCtx) error { return galleryio.ExportGalleryJSON(cx.Handle, w, nil) })
}

func (s *Server) exportGalleryArchive(name, format string, w io.Writer) error {
	return s.exportGallery(name, func(cx *galleryCtx) error { return galleryio.ExportGalleryArchive(cx.Handle, format, w) })
}

func (s *Server) exportGalleryLight(name string, w io.Writer) error {
	return s.exportGallery(name, func(cx *galleryCtx) error { return galleryio.ExportGalleryLight(cx.Handle, w) })
}

func (s *Server) exportGalleryLightManifest(name string, w io.Writer) error {
	return s.exportGallery(name, func(cx *galleryCtx) error { return galleryio.ExportGalleryLightManifest(cx.Handle, w) })
}

// Streams without ctxMu, which would stall a queued switch and every
// request behind it; the pin is taken under the lock so nothing closes
// the gallery first.
func (s *Server) exportGallery(name string, write func(*galleryCtx) error) error {
	s.ctxMu.RLock()
	cx := s.get(name)
	if cx != nil {
		cx.BeginExport()
	}
	s.ctxMu.RUnlock()
	if cx == nil {
		return fmt.Errorf("unknown gallery %q", name)
	}
	defer cx.EndExport()
	return write(cx)
}

func (s *Server) mergeGallery(name, format string, upload io.Reader) (galleryio.MergeResult, error) {
	// Held, not just checked: rename, repoint or remove would close the
	// database mid-merge.
	if err := s.jobs.BeginSchedule(); err != nil {
		return galleryio.MergeResult{}, errJobRunning
	}
	defer s.jobs.EndSchedule()

	cx := s.get(name)
	if cx == nil {
		return galleryio.MergeResult{}, fmt.Errorf("unknown gallery %q", name)
	}
	res, err := galleryio.MergeGallery(cx.Handle, format, upload, s.maxFileSizeMB())
	if err == nil {
		cx.InvalidateCaches()
	}
	return res, err
}

func (s *Server) transferOneImage(srcCx, dstCx *galleryCtx, id int64, removeAfter bool) error {
	return galleryio.TransferOneImage(srcCx.Handle, dstCx.Handle, id, removeAfter,
		s.maxFileSizeMB(), s.onImageDeleteCallback())
}
