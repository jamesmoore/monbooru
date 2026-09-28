package web

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
)

type readerData struct {
	baseData
	Image       models.Image
	Filename    string
	Page        int
	PageCount   int
	NextPage    int
	BackQS      template.URL
	BackKVQS    template.URL
	BackToPages bool
}

type pagesGridData struct {
	baseData
	Image        models.Image
	Filename     string
	PageCount    int
	LastReadPage int
	TagSidebar   tagSidebar
	BackQuery    string
	BackQS       template.URL
	BackKVQS     template.URL
}

// The stored page is re-checked: a re-ingested archive can shrink, and
// the first or last page means unstarted or finished.
func resumePage(img *models.Image) int {
	if img.LastReadPage == nil || img.PageCount == nil {
		return 0
	}
	page := *img.LastReadPage
	if page <= 1 || page >= *img.PageCount {
		return 0
	}
	return page
}

// On false the 404 is already written.
func (s *Server) loadMangaImage(w http.ResponseWriter, r *http.Request) (*models.Image, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFoundHandler(w, r)
		return nil, false
	}
	img, err := loadImage(r.Context(), s.db(), id)
	if err != nil || img.FileType != models.FileTypeCBZ || img.PageCount == nil || *img.PageCount < 1 {
		s.notFoundHandler(w, r)
		return nil, false
	}
	return img, true
}

func (s *Server) readerHandler(w http.ResponseWriter, r *http.Request) {
	img, ok := s.loadMangaImage(w, r)
	if !ok {
		return
	}
	pageCount := *img.PageCount
	// A bare URL resumes; page 1 would clear the bookmark on every visit.
	page := 1
	if resume := resumePage(img); resume > 0 {
		page = resume
	}
	rawPage := ""
	if v := r.URL.Query().Get("page"); v != "" {
		rawPage = v
		if n, err := strconv.Atoi(v); err == nil {
			page = n
		}
	}
	if page < 1 {
		page = 1
	}
	if page > pageCount {
		page = pageCount
	}
	// Redirect before recording, so only canonical URLs move the bookmark.
	if rawPage != "" && rawPage != strconv.Itoa(page) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page))
		http.Redirect(w, r, r.URL.Path+"?"+q.Encode(), http.StatusSeeOther)
		return
	}
	s.recordReaderPosition(img, page, pageCount)
	next := 0
	if page < pageCount {
		next = page + 1
	}
	back := parseBackContext(r)
	backToPages := r.URL.Query().Get("from") == "pages"
	backQS, backKVQS := back.ReaderQS(backToPages)

	data := readerData{
		baseData:    s.base(r, "gallery", filepath.Base(img.CanonicalPath)+" - Reader - "+s.booruName()),
		Image:       *img,
		Filename:    filepath.Base(img.CanonicalPath),
		Page:        page,
		PageCount:   pageCount,
		NextPage:    next,
		BackQS:      backQS,
		BackKVQS:    backKVQS,
		BackToPages: backToPages,
	}
	s.renderTemplate(w, "reader.html", data)
}

// A write on GET is safe: prefetch fetches page bytes, never this render.
func (s *Server) recordReaderPosition(img *models.Image, page, pageCount int) {
	next := 0
	if page > 1 && page < pageCount {
		next = page
	}
	stored := 0
	if img.LastReadPage != nil {
		stored = *img.LastReadPage
	}
	if next == stored {
		return
	}
	var value any
	if next > 0 {
		value = next
	}
	if _, err := s.db().Write.Exec(
		`UPDATE images SET last_read_page = ? WHERE id = ?`, value, img.ID,
	); err != nil {
		logx.Warnf("record reader position for image %d: %v", img.ID, err)
	}
}

func (s *Server) pagesGridHandler(w http.ResponseWriter, r *http.Request) {
	img, ok := s.loadMangaImage(w, r)
	if !ok {
		return
	}
	back := parseBackContext(r)
	// false: the grid's back link goes to the detail page, not to itself.
	backQS, backKVQS := back.ReaderQS(false)
	_, imageTags, _ := s.tagSvc().GetImageTags(img.ID)
	base := s.base(r, "gallery", filepath.Base(img.CanonicalPath)+" - Pages - "+s.booruName())
	data := pagesGridData{
		baseData:     base,
		Image:        *img,
		Filename:     filepath.Base(img.CanonicalPath),
		PageCount:    *img.PageCount,
		LastReadPage: resumePage(img),
		TagSidebar:   s.buildTagSidebar(img.ID, base.CSRFToken, readTagModeCookie(r), imageTags),
		BackQuery:    back.Q,
		BackQS:       backQS,
		BackKVQS:     backKVQS,
	}
	s.renderTemplate(w, "pages.html", data)
}

// Idempotent: a second extract lands on the row already holding the bytes.
func (s *Server) extractMangaPage(w http.ResponseWriter, r *http.Request) {
	img, ok := s.loadMangaImage(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || n > *img.PageCount {
		s.notFoundHandler(w, r)
		return
	}
	fail := func(stage string, err error) {
		logx.Warnf("extract page %d of image %d: %s: %v", n, img.ID, stage, err)
		http.Error(w, "Could not extract this page.", http.StatusInternalServerError)
	}
	cx := s.active()
	if cx == nil {
		fail("gallery", fmt.Errorf("no active gallery"))
		return
	}
	writeDir, naming := s.receivedNaming(cx.Name)
	destDir, err := cx.Boundary().ResolveSubdir(writeDir)
	if err != nil {
		fail("resolve folder", err)
		return
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		fail("create folder", err)
		return
	}
	stem := strings.TrimSuffix(filepath.Base(img.CanonicalPath), filepath.Ext(img.CanonicalPath))
	pageID, filed, err := s.extractMangaPageToGallery(cx, img, n, destDir, stem+"_p")
	if err != nil {
		fail("extract", err)
		return
	}
	if filed {
		if _, err := naming.Apply(r.Context(), cx.DB, cx.Boundary(), pageID, "", ""); err != nil {
			logx.Warnf("extract page %d of image %d: file: %v", n, img.ID, err)
		}
	}
	cx.InvalidateCaches()
	http.Redirect(w, r, fmt.Sprintf("/images/%d", pageID), http.StatusSeeOther)
}

func loadMangaMeta(ctx context.Context, database *db.DB, imageID int64) *models.MangaMetadata {
	var m models.MangaMetadata
	var title, series, number, volume, summary, notes sql.NullString
	var writer, penciller, inker, colorist, letterer sql.NullString
	var coverArtist, editor, publisher, imprint, genre sql.NullString
	var web, languageISO, format, manga, ageRating sql.NullString
	var rawXML sql.NullString
	var count, year, month, day, xmlPageCount sql.NullInt64
	var communityRating sql.NullFloat64
	err := database.Read.QueryRowContext(ctx, `
		SELECT image_id, title, series, number, volume, count, summary, notes,
		       year, month, day, writer, penciller, inker, colorist, letterer, cover_artist, editor, publisher,
		       imprint, genre, web, language_iso, format, manga, age_rating, community_rating, xml_page_count, raw_xml
		FROM manga_metadata WHERE image_id = ?`, imageID,
	).Scan(&m.ImageID, &title, &series, &number, &volume, &count, &summary, &notes,
		&year, &month, &day, &writer, &penciller, &inker, &colorist, &letterer, &coverArtist, &editor, &publisher,
		&imprint, &genre, &web, &languageISO, &format, &manga, &ageRating, &communityRating, &xmlPageCount, &rawXML)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		logx.Debugf("loadMangaMeta: %v", err)
		return nil
	}
	m.Title = nullToString(title)
	m.Series = nullToString(series)
	m.Number = nullToString(number)
	m.Volume = nullToString(volume)
	m.Summary = nullToString(summary)
	m.Notes = nullToString(notes)
	m.Writer = nullToString(writer)
	m.Penciller = nullToString(penciller)
	m.Inker = nullToString(inker)
	m.Colorist = nullToString(colorist)
	m.Letterer = nullToString(letterer)
	m.CoverArtist = nullToString(coverArtist)
	m.Editor = nullToString(editor)
	m.Publisher = nullToString(publisher)
	m.Imprint = nullToString(imprint)
	m.Genre = nullToString(genre)
	m.Web = nullToString(web)
	m.LanguageISO = nullToString(languageISO)
	m.Format = nullToString(format)
	m.Manga = nullToString(manga)
	m.AgeRating = nullToString(ageRating)
	m.RawXML = nullToString(rawXML)
	m.Count = nullToIntPtr(count)
	m.Year = nullToIntPtr(year)
	m.Month = nullToIntPtr(month)
	m.Day = nullToIntPtr(day)
	m.XMLPageCount = nullToIntPtr(xmlPageCount)
	m.CommunityRating = nullToFloatPtr(communityRating)
	return &m
}

func nullToString(n sql.NullString) string {
	if n.Valid {
		return n.String
	}
	return ""
}

func nullToIntPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func nullToFloatPtr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}
