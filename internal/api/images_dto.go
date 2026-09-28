package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/markup"
	"github.com/monbooru/monbooru/internal/models"
)

func validateVia(via string) error {
	if via == "" {
		return nil
	}
	if utf8.RuneCountInString(via) > 200 {
		return fmt.Errorf("via must be 200 characters or less")
	}
	for _, r := range via {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\'', '<', '>', '[', ']', '\\':
			return fmt.Errorf("via must not contain whitespace or any of: \" ' < > [ ] \\")
		}
	}
	return nil
}

const (
	maxImageSourceLen     = gallery.MaxSourceLabelLen
	maxImageURLLen        = gallery.MaxSourceURLLen
	maxImageCommentaryLen = gallery.MaxCommentaryLen
	maxImageOriginalLen   = gallery.MaxOriginalLen
	maxAnnotationBodyLen  = gallery.MaxAnnotationBodyLen
	maxAnnotations        = 500
	maxSourceMD5Len       = 64
	maxSourcePostIDLen    = 64
	maxSourcePostExtLen   = 16
)

func validateMaxLen(field, s string, max int) error {
	if utf8.RuneCountInString(s) > max {
		return fmt.Errorf("%s must be %d characters or less", field, max)
	}
	return nil
}

func validateImageSource(s string) error { return validateMaxLen("source", s, maxImageSourceLen) }

func validateImageURL(s string) error {
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > maxImageURLLen {
		return fmt.Errorf("url must be %d characters or less", maxImageURLLen)
	}
	if !gallery.ValidExternalURL(s) {
		return fmt.Errorf("url must start with http:// or https://")
	}
	return nil
}

func validateImageCollection(s string) error {
	return validateMaxLen("collection", s, maxImageSourceLen)
}

func validateCreateProvenance(source, postID, url, md5, parentURL, collection, commentary, translated, original, postExt string, order *int) error {
	if err := validateImageSource(source); err != nil {
		return err
	}
	if err := validateMaxLen("post_id", postID, maxSourcePostIDLen); err != nil {
		return err
	}
	if err := validateImageURL(url); err != nil {
		return err
	}
	if err := validateImageURL(parentURL); err != nil {
		return fmt.Errorf("parent_url: %v", err)
	}
	if err := validateImageCollection(collection); err != nil {
		return err
	}
	if err := validateMaxLen("md5", md5, maxSourceMD5Len); err != nil {
		return err
	}
	if err := validateMaxLen("commentary", commentary, maxImageCommentaryLen); err != nil {
		return err
	}
	if err := validateMaxLen("commentary_translated", translated, maxImageCommentaryLen); err != nil {
		return err
	}
	if err := validateMaxLen("original", original, maxImageOriginalLen); err != nil {
		return err
	}
	if err := validateMaxLen("post_ext", postExt, maxSourcePostExtLen); err != nil {
		return err
	}
	if order != nil {
		if *order < 1 {
			return fmt.Errorf("collection_order must be 1 or higher")
		}
		if collection == "" {
			return fmt.Errorf("collection_order requires a non-empty collection")
		}
	}
	return nil
}

type imageResponse struct {
	ID                 int64                 `json:"id"`
	SHA256             string                `json:"sha256"`
	MD5                string                `json:"md5"`
	CanonicalPath      string                `json:"canonical_path"`
	Aliases            []string              `json:"aliases"`
	FileType           string                `json:"file_type"`
	Width              *int                  `json:"width"`
	Height             *int                  `json:"height"`
	FileSize           int64                 `json:"file_size"`
	IsFavorited        bool                  `json:"is_favorited"`
	IsInbox            bool                  `json:"is_inbox"`
	IsMissing          bool                  `json:"is_missing"`
	AutoTaggedAt       *time.Time            `json:"auto_tagged_at"`
	SourceType         string                `json:"source_type"`
	Origin             string                `json:"origin"`
	Source             string                `json:"source"`
	URL                string                `json:"url"`
	Note               string                `json:"note"`
	OriginalSource     string                `json:"original_source"`
	PageCount          *int                  `json:"page_count"`
	Series             string                `json:"collection"`
	SeriesOrder        *int                  `json:"collection_order"`
	Collections        []collectionJSON      `json:"collections,omitempty"`
	Sources            []sourceJSON          `json:"sources,omitempty"`
	Annotations        []annotationJSON      `json:"annotations,omitempty"`
	Phash              *string               `json:"phash"`
	IngestedAt         time.Time             `json:"ingested_at"`
	ThumbnailURL       string                `json:"thumbnail_url"`
	Tags               []imageTagJSON        `json:"tags"`
	TagSources         map[string][]string   `json:"tag_sources,omitempty"`
	ScheduledLookup    *bool                 `json:"scheduled_lookup,omitempty"`
	ScheduledLookupPTR *bool                 `json:"scheduled_lookup_ptr,omitempty"`
	Lookup             map[string]lookupJSON `json:"lookup,omitempty"`
}

type lookupJSON struct {
	LastAt     string `json:"last_at,omitempty"`
	LastResult string `json:"last_result,omitempty"`
	Attempts   int    `json:"attempts"`
	NextDueAt  string `json:"next_due_at,omitempty"`
}

type collectionJSON struct {
	Name  string `json:"name"`
	Order *int   `json:"order"`
}

type sourceJSON struct {
	Site                 string  `json:"site"`
	PostID               string  `json:"post_id,omitempty"`
	URL                  string  `json:"url"`
	Commentary           string  `json:"commentary,omitempty"`
	CommentaryTranslated string  `json:"commentary_translated,omitempty"`
	Original             string  `json:"original,omitempty"`
	Similarity           float64 `json:"similarity,omitempty"`
}

// On input the site comes from the pushed source, not the box.
type annotationJSON struct {
	Site     string `json:"site,omitempty"`
	PostID   string `json:"post_id,omitempty"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	W        int    `json:"w"`
	H        int    `json:"h"`
	Body     string `json:"body"`
	BodyHTML string `json:"body_html,omitempty"`
	BodyText string `json:"body_text,omitempty"`
}

// Without it nothing bounds the conversion of a JSON body.
const maxImageCommentaryDTextLen = 4 * maxImageCommentaryLen

// A JSON body has no size limit: a note's HTML is cut before converting.
const maxAnnotationBodyHTMLLen = 4 * maxAnnotationBodyLen

func firstRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// DText wins over the plain rendering: only a booru's own markup still
// says what the artist wrote.
func commentaryFromInput(plain, dtext string) string {
	if strings.TrimSpace(dtext) == "" {
		return strings.TrimSpace(plain)
	}
	body := markup.FromDText(dtext)
	// Converting can grow a body, so the cap applies to what is stored.
	if r := []rune(body); len(r) > maxImageCommentaryLen {
		body = string(r[:maxImageCommentaryLen])
	}
	return body
}

func annotationsFromInput(in []annotationJSON, postURL string) []models.Annotation {
	if len(in) > maxAnnotations {
		in = in[:maxAnnotations]
	}
	var base *url.URL
	if postURL != "" {
		base, _ = url.Parse(postURL)
	}
	out := make([]models.Annotation, 0, len(in))
	for _, n := range in {
		body := n.Body
		if n.BodyHTML != "" {
			body = markup.FromHTML(firstRunes(n.BodyHTML, maxAnnotationBodyHTMLLen), base)
		}
		if r := []rune(body); len(r) > maxAnnotationBodyLen {
			body = string(r[:maxAnnotationBodyLen])
		}
		out = append(out, models.Annotation{
			X: max(n.X, 0), Y: max(n.Y, 0), W: max(n.W, 0), H: max(n.H, 0), Body: body,
		})
	}
	return out
}

func parseNotesField(raw, postURL string) []models.Annotation {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var in []annotationJSON
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil
	}
	return annotationsFromInput(in, postURL)
}

type imageTagJSON struct {
	Name       string   `json:"name"`
	Category   string   `json:"category"`
	IsAuto     bool     `json:"is_auto"`
	Confidence *float64 `json:"confidence"`
	TaggerName *string  `json:"tagger_name"`
}

func makeImageResponse(g Gallery, img models.Image, tags []imageTagJSON, aliases []string) imageResponse {
	if tags == nil {
		tags = []imageTagJSON{}
	}
	if aliases == nil {
		aliases = []string{}
	}
	return imageResponse{
		ID:             img.ID,
		SHA256:         img.SHA256,
		MD5:            img.MD5,
		CanonicalPath:  img.CanonicalPath,
		Aliases:        aliases,
		FileType:       img.FileType,
		Width:          img.Width,
		Height:         img.Height,
		FileSize:       img.FileSize,
		IsFavorited:    img.IsFavorited,
		IsInbox:        img.IsInbox,
		IsMissing:      img.IsMissing,
		AutoTaggedAt:   img.AutoTaggedAt,
		SourceType:     img.SourceType,
		Origin:         img.Origin,
		Source:         img.Source,
		URL:            img.URL,
		Note:           img.Note,
		OriginalSource: img.OriginalSource,
		PageCount:      img.PageCount,
		Series:         img.Series,
		SeriesOrder:    img.SeriesOrder,
		Phash:          phashHexPtr(img.Phash),
		IngestedAt:     img.IngestedAt,
		ThumbnailURL:   "/thumbnails/" + g.Name + "/" + strconv.FormatInt(img.ID, 10) + ".jpg",
		Tags:           tags,
	}
}

func phashHexPtr(p *int64) *string {
	if p == nil {
		return nil
	}
	s := fmt.Sprintf("%016x", uint64(*p))
	return &s
}
