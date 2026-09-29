package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

type sha256DuplicateRow struct {
	ImageID       int64
	CanonicalPath string
	PathID        int64
	AliasPath     string
}

type markedDuplicateRow struct {
	GroupID       int64
	OriginalID    int64
	DuplicateID   int64
	MarkedAt      string
	HasTagsToCopy bool
}

type relationsWalkerData struct {
	baseData
	ActiveGallery string
	Kind          string
	Sha256Rows    []sha256DuplicateRow
	MarkedRows    []markedDuplicateRow
	Total         int
	Page          int
	TotalPages    int
}

func (d relationsWalkerData) HasRows() bool { return len(d.Sha256Rows) > 0 || len(d.MarkedRows) > 0 }

const duplicatesWalkerPageSize = 100

func walkerPageOffset(r *http.Request, total int) (page, totalPages, offset int) {
	page = 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 1 {
		page = p
	}
	totalPages = 1
	if total > 0 {
		totalPages = (total + duplicatesWalkerPageSize - 1) / duplicatesWalkerPageSize
	}
	if page > totalPages {
		page = totalPages
	}
	return page, totalPages, (page - 1) * duplicatesWalkerPageSize
}

func (s *Server) sha256WalkerPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	all, err := duplicatePaths(r, cx)
	if err != nil {
		logx.Warnf("sha256 walker query: %v", err)
		http.Error(w, "load duplicates", http.StatusInternalServerError)
		return
	}
	page, totalPages, offset := walkerPageOffset(r, len(all))
	s.renderTemplate(w, "relations_duplicates_sha256.html", relationsWalkerData{
		baseData:      s.base(r, "relations", "Duplicate files - "+s.booruName()),
		ActiveGallery: s.activeGallery(),
		Kind:          "sha256",
		Sha256Rows:    all[offset:min(offset+duplicatesWalkerPageSize, len(all))],
		Total:         len(all),
		Page:          page,
		TotalPages:    totalPages,
	})
}

func (s *Server) markedWalkerPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	ceiling := resolveCeiling(r, cx)
	from := ` FROM dup_group_members m
		JOIN dup_groups g ON g.id = m.group_id
		WHERE m.image_id != g.original_image_id`
	args := []any{}
	if where, wargs := ceiling.WhereTwo("g.original_image_id", "m.image_id"); where != "" {
		from += ` AND ` + where
		args = append(args, wargs...)
	}
	var total int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*)`+from, args...).Scan(&total); err != nil {
		logx.Warnf("marked walker count: %v", err)
		http.Error(w, "load duplicates", http.StatusInternalServerError)
		return
	}
	page, totalPages, offset := walkerPageOffset(r, total)
	rows, err := cx.DB.Read.Query(
		`SELECT g.id, g.original_image_id, m.image_id, m.created_at`+from+
			` ORDER BY m.created_at DESC, g.id DESC, m.image_id LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), duplicatesWalkerPageSize, offset)...)
	if err != nil {
		logx.Warnf("marked walker query: %v", err)
		http.Error(w, "load duplicates", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rows.Close() }()
	var out []markedDuplicateRow
	for rows.Next() {
		var dr markedDuplicateRow
		var marked string
		if scanErr := rows.Scan(&dr.GroupID, &dr.OriginalID, &dr.DuplicateID, &marked); scanErr != nil {
			logx.Warnf("marked walker scan: %v", scanErr)
			http.Error(w, "scan duplicates", http.StatusInternalServerError)
			return
		}
		dr.MarkedAt = humanISOTime(marked)
		out = append(out, dr)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "iterate duplicates", http.StatusInternalServerError)
		return
	}
	if err := annotateTagsToCopy(cx, out); err != nil {
		logx.Warnf("marked walker tags-to-copy: %v", err)
	}
	s.renderTemplate(w, "relations_duplicates_marked.html", relationsWalkerData{
		baseData:      s.base(r, "relations", "Duplicate images - "+s.booruName()),
		ActiveGallery: s.activeGallery(),
		Kind:          "marked",
		MarkedRows:    out,
		Total:         total,
		Page:          page,
		TotalPages:    totalPages,
	})
}

func annotateTagsToCopy(cx *galleryCtx, rows []markedDuplicateRow) error {
	if len(rows) == 0 {
		return nil
	}
	eligible := map[int64]bool{}
	q, err := cx.DB.Read.Query(`
		SELECT DISTINCT g.id
		FROM dup_groups g
		JOIN dup_group_members m ON m.group_id = g.id
		JOIN image_tags it ON it.image_id = m.image_id
		LEFT JOIN tags t ON t.id = it.tag_id
		LEFT JOIN tag_categories c ON c.id = t.category_id
		WHERE m.image_id != g.original_image_id
		  AND (c.name IS NULL OR c.name != 'rating')
		  AND NOT EXISTS (
		    SELECT 1 FROM image_tags it2
		    WHERE it2.image_id = g.original_image_id AND it2.tag_id = it.tag_id
		  )`)
	if err != nil {
		return err
	}
	defer func() { _ = q.Close() }()
	for q.Next() {
		var gid int64
		if err := q.Scan(&gid); err != nil {
			return err
		}
		eligible[gid] = true
	}
	if err := q.Err(); err != nil {
		return err
	}
	for i := range rows {
		if eligible[rows[i].GroupID] {
			rows[i].HasTagsToCopy = true
		}
	}
	return nil
}

func (s *Server) sha256WalkerRemoveOnePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	pathID, err := strconv.ParseInt(r.FormValue("path_id"), 10, 64)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, "Invalid path id.")
		return
	}
	var aliasPath, canonicalPath string
	var size int64
	if err := s.db().Read.QueryRow(
		`SELECT ip.path, i.canonical_path, i.file_size
		 FROM image_paths ip JOIN images i ON i.id = ip.image_id
		 WHERE ip.id = ? AND ip.is_canonical = 0`,
		pathID,
	).Scan(&aliasPath, &canonicalPath, &size); err != nil {
		flashStatus(w, http.StatusNotFound, "Not a non-canonical path.")
		return
	}
	if err := gallery.DeleteAliasPath(s.db(), pathID); err != nil {
		flashStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if aliasPath != "" {
		if err := unlinkAliasFile(s.boundary(), aliasPath, canonicalPath, size); err != nil {
			logx.Warnf("sha256 walker unlink %q: %v", aliasPath, err)
		}
	}
	redirectWalker(w, r, "sha256")
}

func (s *Server) markedWalkerDeleteOnePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	imageID, err := strconv.ParseInt(r.FormValue("image_id"), 10, 64)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, "Invalid image id.")
		return
	}
	var isOriginal bool
	if err := s.db().Read.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM dup_groups WHERE original_image_id = ?)`, imageID,
	).Scan(&isOriginal); err != nil {
		flashStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if isOriginal {
		flashStatus(w, http.StatusConflict, fmt.Sprintf("Image #%d is its group's original, so it was kept.", imageID))
		return
	}
	if _, err := gallery.DeleteImage(s.db(), s.boundary(), s.thumbnailsPath(), imageID, tags.RemoveAllTagsFromImageTx, s.onImageDeleteCallback()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			flashStatus(w, http.StatusNotFound, fmt.Sprintf("Image #%d is already gone.", imageID))
			return
		}
		flashStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.active().InvalidateCaches()
	redirectWalker(w, r, "marked")
}

// Ceiling-filtered, so delete-all never removes a member the operator
// cannot see.
func (s *Server) markedWalkerDeleteAllPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	q := `
		SELECT m.image_id
		FROM dup_group_members m
		JOIN dup_groups g ON g.id = m.group_id
		WHERE m.image_id != g.original_image_id`
	args := []any{}
	if where, wargs := resolveCeiling(r, cx).WhereTwo("g.original_image_id", "m.image_id"); where != "" {
		q += ` AND ` + where
		args = append(args, wargs...)
	}
	victims, err := db.QueryIDs(cx.DB.Read, q, args...)
	if err != nil {
		flashStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(victims) == 0 {
		writeInlineFlash(w, "ok", "Removed 0 marked duplicate(s).")
		return
	}
	if !s.startJob(w, models.JobTypeDelete) {
		return
	}
	bound := s.boundary()
	thumbnailsPath := s.thumbnailsPath()
	onDelete := s.onImageDeleteCallback()
	go func() {
		ctx := s.jobs.Context()
		total := len(victims)
		s.jobs.Update(0, total, "removing…")
		removed := 0
		for i, id := range victims {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("marked delete-all cancelled (%d/%d)", removed, total))
				s.active().InvalidateCaches()
				return
			}
			if _, err := gallery.DeleteImage(s.db(), bound, thumbnailsPath, id, tags.RemoveAllTagsFromImageTx, onDelete); err != nil {
				logx.Warnf("marked delete-all image %d: %v", id, err)
				continue
			}
			removed++
			if (i+1)%25 == 0 || i == total-1 {
				s.jobs.Update(i+1, total, "removing…")
			}
		}
		s.active().InvalidateCaches()
		s.jobs.Complete(fmt.Sprintf("Removed %d marked duplicate(s).", removed))
	}()
	writeInlineFlash(w, "ok", "Marked duplicate removal started.")
}

func redirectWalker(w http.ResponseWriter, r *http.Request, kind string) {
	target := "/relations/duplicates/" + kind
	hxRedirect(w, r, target)
}
