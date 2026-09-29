package web

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	meta "github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
)

func setFlashHeader(w http.ResponseWriter, text, kind string, extras map[string]any) {
	kind = cmp.Or(kind, "ok")
	// The client renders this through innerHTML, so the text is escaped here.
	triggers := map[string]any{
		"monbooru:flash": map[string]any{"text": html.EscapeString(text), "kind": kind},
	}
	for k, v := range extras {
		triggers[k] = v
	}
	if b, err := json.Marshal(triggers); err == nil {
		w.Header().Set("HX-Trigger", asciiJSON(b))
	}
}

// XHR hands a header to the page one byte per character, so UTF-8 would
// arrive as Latin-1 mojibake; a JSON \u escape arrives intact.
func asciiJSON(b []byte) string {
	var sb strings.Builder
	for _, r := range string(b) {
		switch {
		case r < utf8.RuneSelf:
			sb.WriteRune(r)
		case r > 0xFFFF:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&sb, `\u%04x\u%04x`, hi, lo)
		default:
			fmt.Fprintf(&sb, `\u%04x`, r)
		}
	}
	return sb.String()
}

func hxDone(w http.ResponseWriter, r *http.Request, flash, hxDest, fallback string) {
	if isHTMXRequest(r) {
		setFlashHeader(w, flash, "ok", nil)
		if hxDest == "" {
			w.Header().Set("HX-Refresh", "true")
		} else {
			w.Header().Set("HX-Redirect", hxDest)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, fallback, http.StatusSeeOther)
}

func hxRedirect(w http.ResponseWriter, r *http.Request, dest string) {
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func writeInlineFlash(w http.ResponseWriter, kind, text string) {
	kind = cmp.Or(kind, "ok")
	_, _ = w.Write([]byte(`<div class="flash flash-` + kind + `">` + html.EscapeString(text) + `</div>`))
}

// writeInlineFlashHTML writes body as is: the caller escapes anything
// operator-supplied in it.
func writeInlineFlashHTML(w http.ResponseWriter, kind, body string) {
	kind = cmp.Or(kind, "ok")
	_, _ = w.Write([]byte(`<div class="flash flash-` + kind + `">` + body + `</div>`))
}

func writeFlashOOB(w http.ResponseWriter, id, kind, text string) {
	body := ""
	if text != "" {
		kind = cmp.Or(kind, "ok")
		body = `<div class="flash flash-` + kind + `">` + html.EscapeString(text) + `</div>`
	}
	_, _ = w.Write([]byte(`<div id="` + id + `" hx-swap-oob="true">` + body + `</div>`))
}

func (s *Server) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	if p := strings.TrimRight(r.URL.Path, "/"); p != "" && p != r.URL.Path && localPath(p) {
		if r.URL.RawQuery != "" {
			p += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, p, http.StatusMovedPermanently)
		return
	}
	s.renderNotFound(w, r)
}

// For a subtree route: the mux redirects its slash-less form onto the
// slash, so the slash retry would loop.
func (s *Server) renderNotFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	s.renderTemplate(w, "notfound.html", s.base(r, "", "Not found - "+s.booruName()))
}

func loadImage(ctx context.Context, database *db.DB, id int64) (*models.Image, error) {
	img, err := models.ScanImageRow(database.Read.QueryRowContext(ctx,
		`SELECT `+models.ImageRowColumns+` FROM images i WHERE i.id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &img, nil
}

func loadSDMeta(ctx context.Context, database *db.DB, id int64) *models.SDMetadata {
	var m models.SDMetadata
	var rawParams, genHash *string
	err := database.Read.QueryRowContext(ctx,
		`SELECT image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash
		 FROM sd_metadata WHERE image_id = ?`, id,
	).Scan(&m.ImageID, &m.Prompt, &m.NegativePrompt, &m.Model, &m.Seed, &m.Sampler, &m.Steps, &m.CFGScale, &rawParams, &genHash)
	if err != nil {
		return nil
	}
	if rawParams != nil {
		m.RawParams = *rawParams
	}
	if genHash != nil {
		m.GenerationHash = *genHash
	}
	if m.RawParams != "" {
		m.ParsedParams = meta.ParseAllSDParams(m.RawParams)
	}
	return &m
}

func loadComfyMeta(ctx context.Context, database *db.DB, id int64) *models.ComfyUIMetadata {
	var m models.ComfyUIMetadata
	var genHash *string
	err := database.Read.QueryRowContext(ctx,
		`SELECT image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash
		 FROM comfyui_metadata WHERE image_id = ?`, id,
	).Scan(&m.ImageID, &m.Prompt, &m.ModelCheckpoint, &m.Seed, &m.Sampler, &m.Steps, &m.CFGScale, &m.RawWorkflow, &genHash)
	if err != nil {
		return nil
	}
	if genHash != nil {
		m.GenerationHash = *genHash
	}
	return &m
}

func userAndStaleTags(imageTags []models.ImageTag) (hasUser, hasStale bool) {
	for _, t := range imageTags {
		if !t.IsAuto && t.TaggerName == "" && !t.IsImplied {
			hasUser = true
		}
		if t.Stale {
			hasStale = true
		}
		if hasUser && hasStale {
			break
		}
	}
	return hasUser, hasStale
}

func extraImagePaths(paths []models.ImagePath) int {
	return max(len(paths)-1, 0)
}

func loadImagePaths(ctx context.Context, database *db.DB, b *gallery.Boundary, id int64) []models.ImagePath {
	rows, err := database.Read.QueryContext(ctx,
		`SELECT id, image_id, path, is_canonical FROM image_paths WHERE image_id = ? ORDER BY is_canonical DESC, id`,
		id,
	)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var paths []models.ImagePath
	for rows.Next() {
		var p models.ImagePath
		var isCanon int
		if err := rows.Scan(&p.ID, &p.ImageID, &p.Path, &isCanon); err != nil {
			logx.Warnf("load image paths scan: %v", err)
			continue
		}
		p.IsCanonical = isCanon == 1
		// A gone file is move history, and one outside the boundary is
		// another gallery's; neither is a live duplicate.
		if !p.IsCanonical {
			if _, statErr := os.Stat(p.Path); os.IsNotExist(statErr) || b.Check(p.Path) != nil {
				continue
			}
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		logx.Warnf("load image paths: %v", err)
	}
	return paths
}

// Must match the stylesheet's --bg.
const defaultThemeColor = "#0e0e0e"

func (s *Server) manifestHandler(w http.ResponseWriter, r *http.Request) {
	icon := map[string]any{
		"src":     "/static/icon-192.png",
		"sizes":   "192x192",
		"type":    "image/png",
		"purpose": "any maskable",
	}
	name := s.booruName()
	color := cmp.Or(s.themeColor(), defaultThemeColor)
	w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
	// Built from live config, so a cached copy would keep the old name
	// after a rename.
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":             name,
		"short_name":       name,
		"id":               "/",
		"start_url":        "/",
		"display":          "standalone",
		"background_color": color,
		"theme_color":      color,
		"icons":            []map[string]any{icon},
	})
}
