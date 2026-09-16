package web

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/gallery"
)

// namePreviewRows caps what a keystroke renders: enough to see a sequence
// advance, few enough that typing stays cheap on a large scope. The
// operator can ask for namePreviewRowsMax on a click, which is bounded
// too - a {md5} template hashes files that carry no digest yet, and one
// click must not turn into an unbounded read.
const (
	namePreviewRows    = 5
	namePreviewRowsMax = 25
	// namePreviewFrom is how much of the old path a row shows before the
	// middle is elided, the way elideHash treats a digest. It is what fits
	// the narrower of the two columns at the dialog's declared width, so
	// the CSS ellipsis behind it never has to add a second one.
	namePreviewFrom = 36
)

// namePreviewScopes maps the surface a dialog belongs to onto the parse
// scopes its two fields use, so the preview refuses exactly what the submit
// would.
var namePreviewScopes = map[string]struct{ folder, name gallery.Scope }{
	"place":       {gallery.ScopeMove, gallery.ScopeRename},
	"place-batch": {gallery.ScopeMoveBatch, gallery.ScopeRenameBatch},
}

// namePreview answers what a template would name the given images, so the
// operator reads the result before committing to it. Every dialog and
// settings field renders through this one endpoint rather than a second
// implementation in the browser. Nothing to show answers an empty body
// rather than a 204: htmx does not swap on No Content, which would leave
// the last render standing under a field that no longer says it.
func (s *Server) namePreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("scope") == "upload" {
		s.uploadDestPreview(r.Context(), w, q.Get("folder"), q.Get("name"))
		return
	}
	scope, known := namePreviewScopes[q.Get("scope")]
	if !known {
		return
	}
	rawFolder, wantFolder := strings.TrimSpace(q.Get("folder")), q.Has("folder")
	rawName := strings.TrimSpace(q.Get("name"))
	folderTmpl, err := gallery.ParseNameTemplate(rawFolder, scope.folder)
	if err != nil {
		s.renderNamePreviewError(w, "folder", err.Error())
		return
	}
	var nameTmpl *gallery.NameTemplate
	if scope.name == gallery.ScopeRenameBatch {
		// The job's own entry point, so the preview shows the implicit {n}
		// a plain base name gets instead of the whole scope on one name.
		nameTmpl, err = gallery.ParseBatchRenameTemplate(rawName)
	} else {
		nameTmpl, err = gallery.ParseNameTemplate(rawName, scope.name)
	}
	if err != nil {
		s.renderNamePreviewError(w, "name", err.Error())
		return
	}
	if !wantFolder && nameTmpl == nil {
		return
	}

	want := namePreviewRows
	if n, _ := strconv.Atoi(q.Get("rows")); n > want {
		want = min(n, namePreviewRowsMax)
	}
	ids := s.namePreviewIDs(q["ids"], want)
	if len(ids) == 0 {
		return
	}
	total, _ := strconv.Atoi(q.Get("total"))
	total = max(total, len(ids))

	rows := make([]namePreviewRow, 0, len(ids))
	var rowErr error
	var changed bool
	md5Cap := s.previewMD5Cap()
	// The rows arrive in scope order, so the run's own numbering can be
	// replayed against them: a destination taken on disk or by an earlier row
	// is numbered aside, and a preview the job then rewrites is the one thing
	// the affordance must not do.
	claimed := make(map[string]struct{}, len(ids))
	for i, id := range ids {
		facts, factErr := gallery.LoadNameFacts(r.Context(), s.db(), s.activeGallery(), id, md5Cap, folderTmpl, nameTmpl)
		if factErr != nil {
			rowErr = factErr
			continue
		}
		facts.N, facts.NWidth = i+1, max(len(strconv.Itoa(total)), 2)

		var folder, name *string
		if wantFolder {
			rendered, renderErr := renderOrLiteral(folderTmpl, rawFolder, facts)
			if renderErr != nil {
				rowErr = renderErr
				continue
			}
			// Containment is the move's other refusal, and the preview is
			// where a destination gets checked: submitting an escaping one
			// answers a status htmx discards, so the dialog would just sit
			// there saying nothing.
			if _, resolveErr := gallery.ResolveSubdir(s.galleryPath(), rendered); resolveErr != nil {
				s.renderNamePreviewError(w, "folder", resolveErr.Error())
				return
			}
			folder = &rendered
		}
		if nameTmpl != nil {
			rendered, renderErr := renderOrLiteral(nameTmpl, rawName, facts)
			if renderErr != nil {
				rowErr = renderErr
				continue
			}
			name = &rendered
		}
		dest, _, destErr := gallery.PlannedPath(s.db(), s.galleryPath(), id, folder, name, claimed)
		if destErr != nil {
			rowErr = destErr
			continue
		}
		claimed[dest] = struct{}{}
		// Either half can change, so both sides carry the whole path.
		from := namePath(facts.Folder, facts.Base)
		to := namePath(gallery.FolderPath(s.galleryPath(), dest), filepath.Base(dest))
		changed = changed || from != to
		rows = append(rows, namePreviewRow{From: elideMiddle(from, namePreviewFrom), FromFull: from, To: to})
	}
	if len(rows) == 0 {
		if rowErr != nil {
			s.renderNamePreviewError(w, "", rowErr.Error())
		}
		return
	}
	// A destination that renames and moves nothing is the state the dialog
	// opens in; printing the scope back unchanged is noise.
	if !changed {
		return
	}
	caption := ""
	if total > len(rows) {
		caption = fmt.Sprintf("first %d of %d, in scope order", len(rows), total)
	}
	// Offer more only when there are more to fetch: the caller sends the head
	// of the scope, so a short id list is all there is to show.
	more := len(rows) == want && total > len(rows) && want < namePreviewRowsMax
	// Sample rows show where each one lands, never the shape of the whole
	// run. A folder built on an identity token gives every image one of its
	// own, which is the destination mistake worth naming before it is made.
	note := ""
	if total > 1 && wantFolder && folderTmpl.PerImage() {
		note = fmt.Sprintf("a folder per image, up to %d new ones", total)
	}
	s.renderNamePreview(w, caption, note, rows, more)
}

// elideMiddle keeps both ends of a path, which is what someone checking a
// destination reads: the folder it starts in and the name it ends with.
// The whole value stays one hover away in the row's title. Counted in
// runes, since a filename is in whatever script named it.
func elideMiddle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	head := max / 3
	return string(r[:head]) + "..." + string(r[len(r)-(max-head-3):])
}

// renderOrLiteral resolves a half the way the submit will: singleName and
// batchName both take the literal when the template carries no token of its
// own, and only a render passes through the path tidying.
func renderOrLiteral(tmpl *gallery.NameTemplate, literal string, facts gallery.NameFacts) (string, error) {
	if !tmpl.HasTokens() {
		return literal, nil
	}
	return tmpl.Render(facts)
}

type namePreviewRow struct{ From, FromFull, To string }

// previewMD5Cap bounds the lazy {md5} fill the preview endpoints can
// trigger. They run off a keystroke on ids the caller names, so without
// it typing {md5} into a dialog reads whole files inside the request.
// Over the cap the token renders empty and the backfill job owns the
// digest, matching the detail page's md5 cell.
func (s *Server) previewMD5Cap() int64 { return int64(s.maxFileSizeMB()) * 1024 * 1024 }

// namePath places base under dir. An empty dir is the gallery root, where
// the path is the name on its own.
func namePath(dir, base string) string {
	if dir == "" {
		return base
	}
	return dir + "/" + base
}

func (s *Server) renderNamePreview(w http.ResponseWriter, caption, note string, rows []namePreviewRow, more bool) {
	s.renderTemplate(w, "partials/name_preview.html", map[string]any{
		"Caption":  caption,
		"Note":     note,
		"Rows":     rows,
		"More":     more,
		"NextRows": namePreviewRowsMax,
	})
}

// renderNamePreviewError names the field a refusal came from when one slot
// serves two of them.
func (s *Server) renderNamePreviewError(w http.ResponseWriter, field, msg string) {
	s.renderTemplate(w, "partials/name_preview.html", map[string]any{
		"Field": field,
		"Error": msg,
	})
}

// uploadDestPreview answers the one path a file arriving now would land
// at, so the two destination settings read as the single destination they
// are. A refusal names the field it came from.
func (s *Server) uploadDestPreview(ctx context.Context, w http.ResponseWriter, folder, name string) {
	folderTmpl, err := gallery.ParseNameTemplate(folder, gallery.ScopeUploadFolder)
	if err != nil {
		s.renderNamePreviewError(w, "folder", err.Error())
		return
	}
	nameTmpl, err := gallery.ParseNameTemplate(name, gallery.ScopeUploadName)
	if err != nil {
		s.renderNamePreviewError(w, "name", err.Error())
		return
	}
	ids := s.namePreviewIDs(nil, 1)
	if (folderTmpl == nil && nameTmpl == nil) || len(ids) == 0 {
		return
	}
	facts, err := gallery.LoadNameFacts(ctx, s.db(), s.activeGallery(), ids[0], s.previewMD5Cap(), folderTmpl, nameTmpl)
	if err != nil {
		s.renderNamePreviewError(w, "", err.Error())
		return
	}

	base := facts.Base
	if nameTmpl != nil {
		if base, err = nameTmpl.Render(facts); err != nil {
			s.renderNamePreviewError(w, "name", err.Error())
			return
		}
		if onDisk := filepath.Ext(facts.Base); !strings.EqualFold(filepath.Ext(base), onDisk) {
			base += onDisk
		}
	}
	to := base
	if folderTmpl != nil {
		dir, dirErr := folderTmpl.Render(facts)
		if dirErr != nil {
			s.renderNamePreviewError(w, "folder", dirErr.Error())
			return
		}
		to = namePath(dir, base)
	}
	s.renderNamePreview(w, "a file arriving now", "", []namePreviewRow{{From: facts.Base, FromFull: facts.Base, To: to}}, false)
}

// ingestNaming is where a file found on disk is filed, which is the
// received-file destination behind the operator's opt-in. Empty when the
// opt-in is off, which leaves a dropped file exactly as it arrived.
func (s *Server) ingestNaming(galleryName string) gallery.Naming {
	s.cfgMu.RLock()
	on, folder, name := s.cfg.Gallery.RenameOnIngest, s.cfg.Gallery.DefaultUploadFolder, s.cfg.Gallery.DefaultUploadName
	s.cfgMu.RUnlock()
	if !on {
		return gallery.Naming{}
	}
	return gallery.IngestNaming(galleryName, folder, name)
}

// receivedNaming is where a file monbooru writes itself goes: the directory
// the bytes land in now, and the move that files the row once it exists.
// The name half is deliberately left off - the reader extract, the generated
// pages and the generated cbz each name their own output, and those names
// carry the page order or the generation time.
func (s *Server) receivedNaming(galleryName string) (writeDir string, n gallery.Naming) {
	s.cfgMu.RLock()
	folder := s.cfg.Gallery.DefaultUploadFolder
	s.cfgMu.RUnlock()
	return gallery.ReceivedNaming(galleryName, "", strings.TrimSpace(folder), "")
}

// namePreviewIDs takes the ids the caller named, capped at want, or falls
// back to the newest row so the settings fields have something real to
// render against without the page knowing an id.
func (s *Server) namePreviewIDs(raw []string, want int) []int64 {
	ids := parseIDList(raw)
	if len(ids) > want {
		ids = ids[:want]
	}
	if len(ids) > 0 {
		return ids
	}
	var id int64
	if err := s.db().Read.QueryRow(
		`SELECT id FROM images WHERE is_missing = 0 ORDER BY id DESC LIMIT 1`,
	).Scan(&id); err != nil {
		return nil
	}
	return []int64{id}
}
