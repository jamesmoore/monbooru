package metadata

import (
	"archive/zip"
	"encoding/xml"
	"io"
	"strings"

	"github.com/monbooru/monbooru/internal/models"
)

const ComicInfoMaxRawXML = 64 * 1024

// No Pages element: images.page_count, counted from the archive, is the
// authority.
type comicInfoXML struct {
	XMLName         xml.Name `xml:"ComicInfo"`
	Title           string   `xml:"Title,omitempty"`
	Series          string   `xml:"Series,omitempty"`
	Number          string   `xml:"Number,omitempty"`
	Volume          string   `xml:"Volume,omitempty"`
	Count           *int     `xml:"Count,omitempty"`
	Summary         string   `xml:"Summary,omitempty"`
	Notes           string   `xml:"Notes,omitempty"`
	Year            *int     `xml:"Year,omitempty"`
	Month           *int     `xml:"Month,omitempty"`
	Day             *int     `xml:"Day,omitempty"`
	Writer          string   `xml:"Writer,omitempty"`
	Penciller       string   `xml:"Penciller,omitempty"`
	Inker           string   `xml:"Inker,omitempty"`
	Colorist        string   `xml:"Colorist,omitempty"`
	Letterer        string   `xml:"Letterer,omitempty"`
	CoverArtist     string   `xml:"CoverArtist,omitempty"`
	Editor          string   `xml:"Editor,omitempty"`
	Publisher       string   `xml:"Publisher,omitempty"`
	Imprint         string   `xml:"Imprint,omitempty"`
	Genre           string   `xml:"Genre,omitempty"`
	Web             string   `xml:"Web,omitempty"`
	LanguageISO     string   `xml:"LanguageISO,omitempty"`
	Format          string   `xml:"Format,omitempty"`
	Manga           string   `xml:"Manga,omitempty"`
	AgeRating       string   `xml:"AgeRating,omitempty"`
	CommunityRating *float64 `xml:"CommunityRating,omitempty"`
	PageCount       *int     `xml:"PageCount,omitempty"`
}

// MarshalComicInfo writes no XML declaration, as most archivers do.
func MarshalComicInfo(title string, pageCount int) ([]byte, error) {
	doc := comicInfoXML{Title: title, PageCount: &pageCount}
	return xml.MarshalIndent(doc, "", "  ")
}

func ParseComicInfo(zr *zip.Reader) (*models.MangaMetadata, error) {
	if zr == nil {
		return nil, nil
	}
	var entry *zip.File
	for _, f := range zr.File {
		// The spec puts it at the root; a comicinfo.xml inside a chapter
		// folder is not it.
		if strings.ContainsRune(f.Name, '/') {
			continue
		}
		if strings.EqualFold(f.Name, "ComicInfo.xml") {
			entry = f
			break
		}
	}
	if entry == nil {
		return nil, nil
	}
	rc, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(io.LimitReader(rc, int64(ComicInfoMaxRawXML)+1))
	if err != nil {
		return nil, err
	}
	truncated := len(body) > ComicInfoMaxRawXML
	if truncated {
		body = body[:ComicInfoMaxRawXML]
	}
	var doc comicInfoXML
	// Malformed XML still earns a row, so the raw XML stays inspectable.
	_ = xml.Unmarshal(body, &doc)
	return &models.MangaMetadata{
		Title:           strings.TrimSpace(doc.Title),
		Series:          strings.TrimSpace(doc.Series),
		Number:          strings.TrimSpace(doc.Number),
		Volume:          strings.TrimSpace(doc.Volume),
		Count:           doc.Count,
		Summary:         strings.TrimSpace(doc.Summary),
		Notes:           strings.TrimSpace(doc.Notes),
		Year:            doc.Year,
		Month:           doc.Month,
		Day:             doc.Day,
		Writer:          strings.TrimSpace(doc.Writer),
		Penciller:       strings.TrimSpace(doc.Penciller),
		Inker:           strings.TrimSpace(doc.Inker),
		Colorist:        strings.TrimSpace(doc.Colorist),
		Letterer:        strings.TrimSpace(doc.Letterer),
		CoverArtist:     strings.TrimSpace(doc.CoverArtist),
		Editor:          strings.TrimSpace(doc.Editor),
		Publisher:       strings.TrimSpace(doc.Publisher),
		Imprint:         strings.TrimSpace(doc.Imprint),
		Genre:           strings.TrimSpace(doc.Genre),
		Web:             strings.TrimSpace(doc.Web),
		LanguageISO:     strings.TrimSpace(doc.LanguageISO),
		Format:          strings.TrimSpace(doc.Format),
		Manga:           strings.TrimSpace(doc.Manga),
		AgeRating:       strings.TrimSpace(doc.AgeRating),
		CommunityRating: doc.CommunityRating,
		XMLPageCount:    doc.PageCount,
		RawXML:          string(body),
	}, nil
}
