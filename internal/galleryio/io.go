// Package galleryio exports, imports, merges and transfers whole
// galleries. A restore writes tables directly: the services would fire
// implication fan-outs and recounts the document already carries.
package galleryio

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

// SafeArchiveDest rejects an absolute entry or one that escapes root. It uses
// PathInside, not a ".." substring test, so "foo..bar.ext" stays legal.
func SafeArchiveDest(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute archive entry path")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	dst, err := filepath.Abs(filepath.Join(rootAbs, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	if !gallery.PathInside(rootAbs, dst) {
		return "", fmt.Errorf("archive entry escapes gallery root")
	}
	return dst, nil
}

// Bump ExportVersion when the document gains a field; loadExportIntoDB
// must still read every version down to ExportMinSupported.
const ExportVersion = 12

const ExportMinSupported = 1

// LightManifestVersion does not follow ExportVersion.
const LightManifestVersion = 1

func DecodeExport(r io.Reader) (Export, error) {
	var exp Export
	if err := json.NewDecoder(r).Decode(&exp); err != nil {
		return exp, fmt.Errorf("decode export: %w", err)
	}
	if exp.Version < ExportMinSupported || exp.Version > ExportVersion {
		return exp, fmt.Errorf("unsupported export version %d (supported: %d..%d)", exp.Version, ExportMinSupported, ExportVersion)
	}
	return exp, nil
}

type Export struct {
	Version          int                  `json:"version"`
	GalleryName      string               `json:"gallery_name"`
	GalleryPath      string               `json:"gallery_path"`
	TagCategories    []TagCategoryRow     `json:"tag_categories"`
	Tags             []TagRow             `json:"tags"`
	TagImplications  []TagImplicationRow  `json:"tag_implications"`
	TagNotes         []TagNoteRow         `json:"tag_notes,omitempty"`
	Images           []ImageRow           `json:"images"`
	ImageCollections []ImageCollectionRow `json:"image_collections,omitempty"`
	FindRelations    []FindRelationsRow   `json:"collection_find_relations,omitempty"`
	ImageSources     []ImageSourceRow     `json:"image_sources,omitempty"`
	ImageAnnotations []ImageAnnotationRow `json:"image_annotations,omitempty"`
	ImagePaths       []ImagePathRow       `json:"image_paths"`
	ImageTags        []ImageTagRow        `json:"image_tags"`
	ImageTagSources  []ImageTagSourceRow  `json:"image_tag_sources,omitempty"`
	SDMetadata       []SDMetadataRow      `json:"sd_metadata"`
	ComfyUIMetadata  []ComfyMetadataRow   `json:"comfyui_metadata"`
	MangaMetadata    []MangaMetadataRow   `json:"manga_metadata,omitempty"`
	DupGroups        []DupGroupRow        `json:"dup_groups,omitempty"`
	DupGroupMembers  []DupGroupMemberRow  `json:"dup_group_members,omitempty"`
	AltGroups        []AltGroupRow        `json:"alt_groups,omitempty"`
	AltGroupMembers  []AltGroupMemberRow  `json:"alt_group_members,omitempty"`
	VersionEdges     []VersionEdgeRow     `json:"version_edges,omitempty"`
	DerivativeEdges  []DerivativeEdgeRow  `json:"derivative_edges,omitempty"`
	NotRelatedPairs  []NotRelatedPairRow  `json:"not_related_pairs,omitempty"`
	SavedSearches    []SavedSearchRow     `json:"saved_searches"`
	ImageLookups     []ImageLookupRow     `json:"image_lookups,omitempty"`
}

type TagCategoryRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	IsBuiltin int    `json:"is_builtin"`
}

type TagRow struct {
	ID             int64          `json:"id"`
	Name           string         `json:"name"`
	CategoryID     int64          `json:"category_id"`
	UsageCount     int            `json:"usage_count"`
	IsAlias        int            `json:"is_alias"`
	CanonicalTagID sql.NullInt64  `json:"canonical_tag_id"`
	CreatedAt      string         `json:"created_at"`
	Origin         string         `json:"origin"`
	LastUsedAt     sql.NullString `json:"last_used_at"`
	Stale          int            `json:"stale,omitempty"`
}

type ImageRow struct {
	ID                 int64           `json:"id"`
	SHA256             string          `json:"sha256"`
	MD5                string          `json:"md5,omitempty"`
	CanonicalPath      string          `json:"canonical_path"`
	FolderPath         string          `json:"folder_path"`
	FileType           string          `json:"file_type"`
	Width              sql.NullInt64   `json:"width"`
	Height             sql.NullInt64   `json:"height"`
	FileSize           int64           `json:"file_size"`
	IsMissing          int             `json:"is_missing"`
	IsFavorited        int             `json:"is_favorited"`
	IsInbox            int             `json:"is_inbox"`
	AutoTaggedAt       sql.NullString  `json:"auto_tagged_at"`
	SourceType         string          `json:"source_type"`
	Origin             string          `json:"origin"`
	Source             string          `json:"source"`
	URL                string          `json:"url"`
	PageCount          sql.NullInt64   `json:"page_count,omitempty"`
	DurationSeconds    sql.NullFloat64 `json:"duration_seconds,omitempty"`
	Series             string          `json:"collection,omitempty"`
	SeriesOrder        sql.NullInt64   `json:"collection_order,omitempty"`
	Note               string          `json:"note,omitempty"`
	OriginalSource     string          `json:"original_source,omitempty"`
	Phash              sql.NullInt64   `json:"phash,omitempty"`
	LastReadPage       sql.NullInt64   `json:"last_read_page,omitempty"`
	UploadBatch        sql.NullInt64   `json:"upload_batch,omitempty"`
	IngestedAt         string          `json:"ingested_at"`
	ScheduledLookup    int             `json:"scheduled_lookup"`
	ScheduledLookupPTR int             `json:"scheduled_lookup_ptr"`
}

type FindRelationsRow struct {
	Name string `json:"name"`
}

type ImageLookupRow struct {
	ImageID    int64          `json:"image_id"`
	Backend    string         `json:"backend"`
	Attempts   int            `json:"attempts"`
	QueuedAt   sql.NullString `json:"queued_at"`
	JobID      sql.NullInt64  `json:"job_id"`
	LastAt     sql.NullString `json:"last_at"`
	LastResult string         `json:"last_result"`
	NextDueAt  sql.NullString `json:"next_due_at"`
	PTRCursor  sql.NullInt64  `json:"ptr_cursor"`
}

type ImageCollectionRow struct {
	ImageID  int64         `json:"image_id"`
	Name     string        `json:"name"`
	Position sql.NullInt64 `json:"position"`
}

type ImageSourceRow struct {
	ImageID     int64   `json:"image_id"`
	Site        string  `json:"site"`
	PostID      string  `json:"post_id"`
	URL         string  `json:"url"`
	MD5         string  `json:"md5"`
	Commentary  string  `json:"commentary"`
	Translated  string  `json:"commentary_translated,omitempty"`
	Original    string  `json:"original,omitempty"`
	Similarity  float64 `json:"similarity,omitempty"`
	MD5Match    string  `json:"md5_match,omitempty"`
	ParentURL   string  `json:"parent_url,omitempty"`
	UpgradeKept int     `json:"upgrade_kept,omitempty"`
	PostWidth   int64   `json:"post_width,omitempty"`
	PostHeight  int64   `json:"post_height,omitempty"`
	PostSize    int64   `json:"post_size,omitempty"`
	PostExt     string  `json:"post_ext,omitempty"`
	FetchedAt   string  `json:"fetched_at"`
}

type ImageAnnotationRow struct {
	ImageID   int64  `json:"image_id"`
	Site      string `json:"site"`
	PostID    string `json:"post_id"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	W         int    `json:"w"`
	H         int    `json:"h"`
	Body      string `json:"body"`
	Manual    int    `json:"manual,omitempty"`
	FetchedAt string `json:"fetched_at"`
}

type ImagePathRow struct {
	ID          int64  `json:"id"`
	ImageID     int64  `json:"image_id"`
	Path        string `json:"path"`
	IsCanonical int    `json:"is_canonical"`
	MtimeUnix   int64  `json:"mtime_unix,omitempty"`
	MtimeNsec   int64  `json:"mtime_nsec,omitempty"`
}

type ImageTagRow struct {
	ImageID    int64           `json:"image_id"`
	TagID      int64           `json:"tag_id"`
	IsAuto     int             `json:"is_auto"`
	IsImplied  int             `json:"is_implied,omitempty"`
	Confidence sql.NullFloat64 `json:"confidence"`
	TaggerName sql.NullString  `json:"tagger_name"`
	CreatedAt  string          `json:"created_at"`
	Stale      int             `json:"stale,omitempty"`
}

// ImageTagSourceRow travels on its own: image_tags.tagger_name keeps only
// the first source, so the ledger can't be rebuilt from it.
type ImageTagSourceRow struct {
	ImageID   int64  `json:"image_id"`
	TagID     int64  `json:"tag_id"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
}

type TagImplicationRow struct {
	ParentTagID  int64  `json:"parent_tag_id"`
	ImpliedTagID int64  `json:"implied_tag_id"`
	CreatedAt    string `json:"created_at"`
	Origin       string `json:"origin"`
	Stale        int    `json:"stale,omitempty"`
}

type TagNoteRow struct {
	TagID int64  `json:"tag_id"`
	Body  string `json:"body"`
	Links string `json:"links"`
}

type SDMetadataRow struct {
	ImageID        int64           `json:"image_id"`
	Prompt         sql.NullString  `json:"prompt"`
	NegativePrompt sql.NullString  `json:"negative_prompt"`
	Model          sql.NullString  `json:"model"`
	Seed           sql.NullInt64   `json:"seed"`
	Sampler        sql.NullString  `json:"sampler"`
	Steps          sql.NullInt64   `json:"steps"`
	CFGScale       sql.NullFloat64 `json:"cfg_scale"`
	RawParams      sql.NullString  `json:"raw_params"`
	GenerationHash sql.NullString  `json:"generation_hash"`
}

type ComfyMetadataRow struct {
	ImageID         int64           `json:"image_id"`
	Prompt          sql.NullString  `json:"prompt"`
	ModelCheckpoint sql.NullString  `json:"model_checkpoint"`
	Seed            sql.NullInt64   `json:"seed"`
	Sampler         sql.NullString  `json:"sampler"`
	Steps           sql.NullInt64   `json:"steps"`
	CFGScale        sql.NullFloat64 `json:"cfg_scale"`
	RawWorkflow     sql.NullString  `json:"raw_workflow"`
	GenerationHash  sql.NullString  `json:"generation_hash"`
}

type MangaMetadataRow struct {
	ImageID         int64           `json:"image_id"`
	Title           sql.NullString  `json:"title"`
	Series          sql.NullString  `json:"series"`
	Number          sql.NullString  `json:"number"`
	Volume          sql.NullString  `json:"volume"`
	Count           sql.NullInt64   `json:"count"`
	Summary         sql.NullString  `json:"summary"`
	Notes           sql.NullString  `json:"notes"`
	Year            sql.NullInt64   `json:"year"`
	Month           sql.NullInt64   `json:"month"`
	Day             sql.NullInt64   `json:"day"`
	Writer          sql.NullString  `json:"writer"`
	Penciller       sql.NullString  `json:"penciller"`
	Inker           sql.NullString  `json:"inker"`
	Colorist        sql.NullString  `json:"colorist"`
	Letterer        sql.NullString  `json:"letterer"`
	CoverArtist     sql.NullString  `json:"cover_artist"`
	Editor          sql.NullString  `json:"editor"`
	Publisher       sql.NullString  `json:"publisher"`
	Imprint         sql.NullString  `json:"imprint"`
	Genre           sql.NullString  `json:"genre"`
	Web             sql.NullString  `json:"web"`
	LanguageISO     sql.NullString  `json:"language_iso"`
	Format          sql.NullString  `json:"format"`
	Manga           sql.NullString  `json:"manga"`
	AgeRating       sql.NullString  `json:"age_rating"`
	CommunityRating sql.NullFloat64 `json:"community_rating"`
	XMLPageCount    sql.NullInt64   `json:"xml_page_count"`
	RawXML          sql.NullString  `json:"raw_xml"`
}

type SavedSearchRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Query     string `json:"query"`
	Sort      string `json:"sort"`
	Order     string `json:"sort_order"`
	Seed      string `json:"seed"`
	CreatedAt string `json:"created_at"`
}

type DupGroupRow struct {
	ID              int64  `json:"id"`
	OriginalImageID int64  `json:"original_image_id"`
	CreatedAt       string `json:"created_at"`
}

type DupGroupMemberRow struct {
	ImageID   int64  `json:"image_id"`
	GroupID   int64  `json:"group_id"`
	CreatedAt string `json:"created_at"`
}

type AltGroupRow struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
}

type AltGroupMemberRow struct {
	ImageID   int64  `json:"image_id"`
	GroupID   int64  `json:"group_id"`
	CreatedAt string `json:"created_at"`
}

type VersionEdgeRow struct {
	ChildImageID  int64  `json:"child_image_id"`
	ParentImageID int64  `json:"parent_image_id"`
	CreatedAt     string `json:"created_at"`
}

type DerivativeEdgeRow struct {
	DerivativeImageID int64  `json:"derivative_image_id"`
	SourceImageID     int64  `json:"source_image_id"`
	CreatedAt         string `json:"created_at"`
}

type NotRelatedPairRow struct {
	AImageID  int64  `json:"a_image_id"`
	BImageID  int64  `json:"b_image_id"`
	CreatedAt string `json:"created_at"`
}

// VACUUM INTO gives a consistent, WAL-folded snapshot of the live gallery,
// and it only reads, so the read pool runs it and writes carry on.
func ExportGalleryDB(cx gallery.Handle, w io.Writer) error {
	tmp, err := os.CreateTemp(filepath.Dir(cx.DBPath), "export-*.db")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := cx.DB.Read.Exec("VACUUM INTO ?", tmpPath); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}

// The ids ride as one json_each parameter, since a scope is a whole search
// and SQLite caps parameters at 32766. nil means the whole gallery.
type exportScope struct {
	ids  []int64
	json string
}

func newExportScope(ids []int64) exportScope {
	if ids == nil {
		return exportScope{}
	}
	b, _ := json.Marshal(ids)
	return exportScope{ids: ids, json: string(b)}
}

func (sc exportScope) where(col string) (string, []any) {
	if sc.ids == nil {
		return "", nil
	}
	return " WHERE " + col + " IN (SELECT value FROM json_each(?))", []any{sc.json}
}

// nil ids exports the whole gallery. A scoped export still carries the
// whole tag catalog, which its images reference.
func ExportGalleryJSON(cx gallery.Handle, w io.Writer, ids []int64) error {
	// One snapshot: a row committed between two sections would dangle in the import.
	tx, err := cx.DB.Read.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	sc := newExportScope(ids)
	var scopeWhere string
	var scopeArgs []any

	bw := newJSONWriter(w)
	bw.objStart()
	bw.field("version", ExportVersion)
	bw.field("gallery_name", cx.Name)
	bw.field("gallery_path", cx.GalleryPath)

	streamRows(bw, "tag_categories", tx,
		`SELECT id, name, color, is_builtin FROM tag_categories ORDER BY id`,
		scanRow(func(r *TagCategoryRow) []any { return []any{&r.ID, &r.Name, &r.Color, &r.IsBuiltin} }))
	streamRows(bw, "tags", tx,
		`SELECT id, name, category_id, usage_count, is_alias, canonical_tag_id, created_at, origin, last_used_at, stale FROM tags ORDER BY id`,
		scanRow(func(r *TagRow) []any {
			return []any{&r.ID, &r.Name, &r.CategoryID, &r.UsageCount, &r.IsAlias, &r.CanonicalTagID, &r.CreatedAt, &r.Origin, &r.LastUsedAt, &r.Stale}
		}))
	streamRows(bw, "tag_implications", tx,
		`SELECT parent_tag_id, implied_tag_id, created_at, origin, stale FROM tag_implications ORDER BY parent_tag_id, implied_tag_id`,
		scanRow(func(r *TagImplicationRow) []any {
			return []any{&r.ParentTagID, &r.ImpliedTagID, &r.CreatedAt, &r.Origin, &r.Stale}
		}))
	streamRows(bw, "tag_notes", tx,
		`SELECT tag_id, body, links FROM tag_notes ORDER BY tag_id`,
		scanRow(func(r *TagNoteRow) []any { return []any{&r.TagID, &r.Body, &r.Links} }))
	scopeWhere, scopeArgs = sc.where("id")
	streamRows(bw, "images", tx,
		`SELECT id, sha256, md5, canonical_path, folder_path, file_type, width, height,
		        file_size, is_missing, is_favorited, is_inbox, auto_tagged_at, source_type, origin, source, url, page_count, duration_seconds, series, series_order, note, original_source,
		        phash, last_read_page, upload_batch, scheduled_lookup, scheduled_lookup_ptr, ingested_at
		 FROM images`+scopeWhere+` ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r ImageRow
			err := rows.Scan(&r.ID, &r.SHA256, &r.MD5, &r.CanonicalPath, &r.FolderPath, &r.FileType,
				&r.Width, &r.Height, &r.FileSize, &r.IsMissing, &r.IsFavorited, &r.IsInbox,
				&r.AutoTaggedAt, &r.SourceType, &r.Origin, &r.Source, &r.URL, &r.PageCount, &r.DurationSeconds, &r.Series, &r.SeriesOrder, &r.Note, &r.OriginalSource,
				&r.Phash, &r.LastReadPage, &r.UploadBatch, &r.ScheduledLookup, &r.ScheduledLookupPTR, &r.IngestedAt)
			return r, err
		}, scopeArgs...)
	scopeWhere, scopeArgs = sc.where("image_id")
	streamRows(bw, "image_collections", tx,
		`SELECT image_id, name, position FROM image_collections`+scopeWhere+` ORDER BY image_id, name`,
		scanRow(func(r *ImageCollectionRow) []any { return []any{&r.ImageID, &r.Name, &r.Position} }), scopeArgs...)
	streamRows(bw, "collection_find_relations", tx,
		`SELECT name FROM collection_find_relations ORDER BY name`,
		scanRow(func(r *FindRelationsRow) []any { return []any{&r.Name} }))
	scopeWhere, scopeArgs = sc.where("image_id")
	streamRows(bw, "image_sources", tx,
		`SELECT image_id, site, post_id, url, md5, commentary, commentary_translated, original, similarity,
		        md5_match, parent_url, upgrade_kept, post_width, post_height, post_size, post_ext, fetched_at
		 FROM image_sources`+scopeWhere+` ORDER BY rowid`,
		func(rows *sql.Rows) (any, error) {
			var r ImageSourceRow
			err := rows.Scan(&r.ImageID, &r.Site, &r.PostID, &r.URL, &r.MD5, &r.Commentary, &r.Translated, &r.Original, &r.Similarity,
				&r.MD5Match, &r.ParentURL, &r.UpgradeKept, &r.PostWidth, &r.PostHeight, &r.PostSize, &r.PostExt, &r.FetchedAt)
			return r, err
		}, scopeArgs...)
	scopeWhere, scopeArgs = sc.where("image_id")
	streamRows(bw, "image_annotations", tx,
		`SELECT image_id, site, post_id, x, y, w, h, body, manual, fetched_at FROM image_annotations`+scopeWhere+` ORDER BY id`,
		scanRow(func(r *ImageAnnotationRow) []any {
			return []any{&r.ImageID, &r.Site, &r.PostID, &r.X, &r.Y, &r.W, &r.H, &r.Body, &r.Manual, &r.FetchedAt}
		}), scopeArgs...)
	scopeWhere, scopeArgs = sc.where("image_id")
	streamRows(bw, "image_paths", tx,
		`SELECT id, image_id, path, is_canonical, mtime_unix, mtime_nsec FROM image_paths`+scopeWhere+` ORDER BY id`,
		scanRow(func(r *ImagePathRow) []any {
			return []any{&r.ID, &r.ImageID, &r.Path, &r.IsCanonical, &r.MtimeUnix, &r.MtimeNsec}
		}), scopeArgs...)
	scopeWhere, scopeArgs = sc.where("image_id")
	streamRows(bw, "image_tags", tx,
		`SELECT image_id, tag_id, is_auto, is_implied, confidence, tagger_name, created_at, stale FROM image_tags`+scopeWhere,
		scanRow(func(r *ImageTagRow) []any {
			return []any{&r.ImageID, &r.TagID, &r.IsAuto, &r.IsImplied, &r.Confidence, &r.TaggerName, &r.CreatedAt, &r.Stale}
		}), scopeArgs...)
	scopeWhere, scopeArgs = sc.where("image_id")
	streamRows(bw, "image_tag_sources", tx,
		`SELECT image_id, tag_id, source, created_at FROM image_tag_sources`+scopeWhere+` ORDER BY image_id, tag_id, source`,
		scanRow(func(r *ImageTagSourceRow) []any { return []any{&r.ImageID, &r.TagID, &r.Source, &r.CreatedAt} }), scopeArgs...)
	scopeWhere, scopeArgs = sc.where("image_id")
	streamRows(bw, "sd_metadata", tx,
		`SELECT image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash FROM sd_metadata`+scopeWhere,
		func(rows *sql.Rows) (any, error) {
			var r SDMetadataRow
			err := rows.Scan(&r.ImageID, &r.Prompt, &r.NegativePrompt, &r.Model, &r.Seed,
				&r.Sampler, &r.Steps, &r.CFGScale, &r.RawParams, &r.GenerationHash)
			return r, err
		}, scopeArgs...)
	scopeWhere, scopeArgs = sc.where("image_id")
	streamRows(bw, "comfyui_metadata", tx,
		`SELECT image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash FROM comfyui_metadata`+scopeWhere,
		func(rows *sql.Rows) (any, error) {
			var r ComfyMetadataRow
			err := rows.Scan(&r.ImageID, &r.Prompt, &r.ModelCheckpoint, &r.Seed,
				&r.Sampler, &r.Steps, &r.CFGScale, &r.RawWorkflow, &r.GenerationHash)
			return r, err
		}, scopeArgs...)
	scopeWhere, scopeArgs = sc.where("image_id")
	streamRows(bw, "manga_metadata", tx,
		`SELECT image_id, title, series, number, volume, count, summary, notes,
		        year, month, day, writer, penciller, inker, colorist, letterer, cover_artist, editor, publisher,
		        imprint, genre, web, language_iso, format, manga, age_rating, community_rating, xml_page_count, raw_xml
		 FROM manga_metadata`+scopeWhere,
		func(rows *sql.Rows) (any, error) {
			var r MangaMetadataRow
			err := rows.Scan(&r.ImageID, &r.Title, &r.Series, &r.Number, &r.Volume, &r.Count, &r.Summary, &r.Notes,
				&r.Year, &r.Month, &r.Day, &r.Writer, &r.Penciller, &r.Inker, &r.Colorist, &r.Letterer, &r.CoverArtist,
				&r.Editor, &r.Publisher, &r.Imprint, &r.Genre, &r.Web, &r.LanguageISO, &r.Format, &r.Manga, &r.AgeRating,
				&r.CommunityRating, &r.XMLPageCount, &r.RawXML)
			return r, err
		}, scopeArgs...)
	streamRows(bw, "dup_groups", tx,
		`SELECT id, original_image_id, created_at FROM dup_groups ORDER BY id`,
		scanRow(func(r *DupGroupRow) []any { return []any{&r.ID, &r.OriginalImageID, &r.CreatedAt} }))
	scopeWhere, scopeArgs = sc.where("image_id")
	streamRows(bw, "dup_group_members", tx,
		`SELECT image_id, group_id, created_at FROM dup_group_members`+scopeWhere+` ORDER BY image_id`,
		scanRow(func(r *DupGroupMemberRow) []any { return []any{&r.ImageID, &r.GroupID, &r.CreatedAt} }), scopeArgs...)
	streamRows(bw, "alt_groups", tx,
		`SELECT id, created_at FROM alt_groups ORDER BY id`,
		scanRow(func(r *AltGroupRow) []any { return []any{&r.ID, &r.CreatedAt} }))
	scopeWhere, scopeArgs = sc.where("image_id")
	streamRows(bw, "alt_group_members", tx,
		`SELECT image_id, group_id, created_at FROM alt_group_members`+scopeWhere+` ORDER BY image_id`,
		scanRow(func(r *AltGroupMemberRow) []any { return []any{&r.ImageID, &r.GroupID, &r.CreatedAt} }), scopeArgs...)
	scopeWhere, scopeArgs = sc.where("child_image_id")
	streamRows(bw, "version_edges", tx,
		`SELECT child_image_id, parent_image_id, created_at FROM version_edges`+scopeWhere+` ORDER BY child_image_id`,
		scanRow(func(r *VersionEdgeRow) []any { return []any{&r.ChildImageID, &r.ParentImageID, &r.CreatedAt} }), scopeArgs...)
	scopeWhere, scopeArgs = sc.where("derivative_image_id")
	streamRows(bw, "derivative_edges", tx,
		`SELECT derivative_image_id, source_image_id, created_at FROM derivative_edges`+scopeWhere+` ORDER BY derivative_image_id, source_image_id`,
		scanRow(func(r *DerivativeEdgeRow) []any { return []any{&r.DerivativeImageID, &r.SourceImageID, &r.CreatedAt} }), scopeArgs...)
	scopeWhere, scopeArgs = sc.where("a_image_id")
	streamRows(bw, "not_related_pairs", tx,
		`SELECT a_image_id, b_image_id, created_at FROM not_related_pairs`+scopeWhere+` ORDER BY a_image_id, b_image_id`,
		scanRow(func(r *NotRelatedPairRow) []any { return []any{&r.AImageID, &r.BImageID, &r.CreatedAt} }), scopeArgs...)
	streamRows(bw, "saved_searches", tx,
		`SELECT id, name, query, sort, sort_order, seed, created_at FROM saved_searches ORDER BY id`,
		scanRow(func(r *SavedSearchRow) []any {
			return []any{&r.ID, &r.Name, &r.Query, &r.Sort, &r.Order, &r.Seed, &r.CreatedAt}
		}))
	scopeWhere, scopeArgs = sc.where("image_id")
	streamRows(bw, "image_lookups", tx,
		`SELECT image_id, backend, attempts, queued_at, job_id, last_at, last_result, next_due_at, ptr_cursor
		 FROM image_lookups`+scopeWhere+` ORDER BY image_id, backend`,
		scanRow(func(r *ImageLookupRow) []any {
			return []any{&r.ImageID, &r.Backend, &r.Attempts, &r.QueuedAt, &r.JobID, &r.LastAt, &r.LastResult, &r.NextDueAt, &r.PTRCursor}
		}), scopeArgs...)
	bw.objEnd()
	return bw.err
}

func ExportGalleryArchive(cx gallery.Handle, format string, w io.Writer) error {
	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	header := &zip.FileHeader{Method: zip.Deflate}
	switch format {
	case "db":
		header.Name = "monbooru.db"
	case "json":
		header.Name = "monbooru.json"
	default:
		return fmt.Errorf("unknown archive format %q", format)
	}
	inner, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	switch format {
	case "db":
		if err := ExportGalleryDB(cx, inner); err != nil {
			return err
		}
	case "json":
		if err := ExportGalleryJSON(cx, inner, nil); err != nil {
			return err
		}
	}

	if err := writeGalleryFilesToZip(zw, cx.Boundary()); err != nil {
		return err
	}
	// Close writes the central directory, so its error must reach the
	// caller; the deferred Close covers the error paths.
	return zw.Close()
}

// Files go in as Store: images are already compressed. An unreadable root
// is skipped so the db/json still exports.
func writeGalleryFilesToZip(zw *zip.Writer, b *gallery.Boundary) error {
	galleryPath := b.Root()
	if _, err := os.Stat(galleryPath); err != nil {
		logx.Warnf("export: gallery path %q unreadable; archive will not include gallery files: %v", galleryPath, err)
		return nil
	}
	return gallery.WalkTree(b, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			logx.Warnf("export: skip %q: %v", path, walkErr)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(galleryPath, path)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			logx.Warnf("export: skip %q: %v", path, err)
			return nil
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			logx.Warnf("export: skip %q: %v", path, err)
			return nil
		}
		fh := &zip.FileHeader{
			Name:   "gallery/" + filepath.ToSlash(rel),
			Method: zip.Store,
		}
		fh.Modified = info.ModTime()
		entry, err := zw.CreateHeader(fh)
		if err != nil {
			return err
		}
		_, err = io.Copy(entry, f)
		return err
	})
}

// The caller closes the target before ApplyImport, so no watcher ingests
// what it extracts, and reopens it after. maxFileSizeMB <= 0 lifts the
// per-entry cap; the count is the entries b excludes.
func ApplyImport(format, tmpPath, dbPath, thumbsPath string, b *gallery.Boundary, maxFileSizeMB int) (int, error) {
	switch format {
	case "db":
		return 0, replaceDBFromFile(tmpPath, dbPath, thumbsPath, b)
	case "json":
		isLight, err := isLightManifestJSON(tmpPath)
		if err != nil {
			return 0, fmt.Errorf("inspect json: %w", err)
		}
		if isLight {
			return 0, replaceFromLightManifest(tmpPath, dbPath, thumbsPath, b)
		}
		return 0, replaceDBFromJSON(tmpPath, dbPath, thumbsPath, b)
	case "zip":
		return replaceFromArchive(tmpPath, dbPath, thumbsPath, b, maxFileSizeMB)
	}
	return 0, fmt.Errorf("unknown import format %q", format)
}

func isLightManifestJSON(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	var probe struct {
		GalleryName   string            `json:"gallery_name"`
		TagCategories []json.RawMessage `json:"tag_categories"`
	}
	if err := json.NewDecoder(f).Decode(&probe); err != nil {
		return false, fmt.Errorf("decode: %w", err)
	}
	return probe.GalleryName == "" && len(probe.TagCategories) == 0, nil
}

func replaceDBFromFile(srcPath, dbPath, thumbsPath string, b *gallery.Boundary) error {
	if err := validateSQLiteFile(srcPath); err != nil {
		return fmt.Errorf("uploaded file is not a valid monbooru database: %w", err)
	}
	return loadThenInstall(srcPath, dbPath, thumbsPath, func(database *db.DB) error {
		// Bootstrap first: a sanitiser may read a column an older snapshot lacks.
		if err := db.Bootstrap(database); err != nil {
			return fmt.Errorf("bootstrap imported db: %w", err)
		}
		if err := sanitizeImportedCategoryColors(database); err != nil {
			return fmt.Errorf("sanitize colors: %w", err)
		}
		if err := sanitizeImportedAliasChains(database); err != nil {
			return fmt.Errorf("sanitize alias chains: %w", err)
		}
		return rebaseImagePaths(database, b)
	})
}

// The database at path is loaded and swapped in for dbPath only on
// success, so a failed load leaves the gallery untouched.
func loadThenInstall(path, dbPath, thumbsPath string, load func(*db.DB) error) error {
	defer func() {
		for _, p := range []string{path + "-wal", path + "-shm"} {
			_ = os.Remove(p)
		}
	}()
	database, err := db.Open(path)
	if err != nil {
		return fmt.Errorf("open new db: %w", err)
	}
	loadErr := load(database)
	if loadErr == nil {
		// Fold the WAL in so the rename below carries every committed page.
		if _, err := database.Write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			loadErr = fmt.Errorf("checkpoint: %w", err)
		}
	}
	if err := database.Close(); err != nil && loadErr == nil {
		loadErr = fmt.Errorf("close new db: %w", err)
	}
	if loadErr != nil {
		return loadErr
	}
	if err := resetDBAndThumbs(dbPath, thumbsPath); err != nil {
		return err
	}
	if err := os.Rename(path, dbPath); err != nil {
		return fmt.Errorf("install db: %w", err)
	}
	return nil
}

// A .db import skips every validator, so its colors are coerced after the fact.
func sanitizeImportedCategoryColors(database *db.DB) error {
	type row struct {
		id    int64
		color string
	}
	all, err := db.QueryAll(database.Read, func(rows *sql.Rows) (row, error) {
		var r row
		err := rows.Scan(&r.id, &r.color)
		return r, err
	}, `SELECT id, color FROM tag_categories`)
	if err != nil {
		return err
	}
	for _, r := range all {
		if tags.IsValidCategoryColor(r.color) {
			continue
		}
		safe := tags.SafeCategoryColor(r.color)
		logx.Warnf("import: replaced invalid color %q on tag_category id=%d with %s", r.color, r.id, safe)
		if _, err := database.Write.Exec(
			`UPDATE tag_categories SET color = ? WHERE id = ?`, safe, r.id,
		); err != nil {
			return err
		}
	}
	return nil
}

// Resolvers follow COALESCE(canonical_tag_id, id) once, without an
// is_alias check, so an imported chain drops out of search and a plain
// tag's pointer misdirects it.
func sanitizeImportedAliasChains(database *db.DB) error {
	type node struct {
		alias     bool
		canonical int64
	}
	type idNode struct {
		id int64
		node
	}
	all, err := db.QueryAll(database.Read, func(rows *sql.Rows) (idNode, error) {
		var n idNode
		var alias int
		err := rows.Scan(&n.id, &alias, &n.canonical)
		n.alias = alias == 1
		return n, err
	}, `SELECT id, is_alias, COALESCE(canonical_tag_id, 0) FROM tags`)
	if err != nil {
		return err
	}
	nodes := make(map[int64]node, len(all))
	ids := make([]int64, 0, len(all))
	for _, n := range all {
		nodes[n.id] = n.node
		ids = append(ids, n.id)
	}
	// Sorted walk order keeps the promoted cycle member deterministic.
	slices.Sort(ids)

	// root[id] is where id's chain ends; a promoted row is its own root.
	root := make(map[int64]int64)
	promoted := make(map[int64]bool)
	for _, id := range ids {
		if n := nodes[id]; !n.alias {
			continue
		}
		if _, done := root[id]; done {
			continue
		}
		var path []int64
		onPath := make(map[int64]bool)
		cur := id
		var end int64
		for {
			n := nodes[cur]
			if !n.alias {
				end = cur
				break
			}
			if r, ok := root[cur]; ok {
				end = r
				break
			}
			if _, ok := nodes[n.canonical]; !ok || onPath[cur] {
				end = cur
				promoted[cur] = true
				break
			}
			onPath[cur] = true
			path = append(path, cur)
			cur = n.canonical
		}
		root[end] = end
		for _, p := range path {
			root[p] = end
		}
	}

	for _, id := range ids {
		n := nodes[id]
		switch {
		case !n.alias && n.canonical != 0:
			if _, err := database.Write.Exec(
				`UPDATE tags SET canonical_tag_id = NULL WHERE id = ?`, id,
			); err != nil {
				return err
			}
			logx.Warnf("import: cleared the canonical pointer on plain tag id=%d", id)
		case !n.alias:
		case promoted[id]:
			if _, err := database.Write.Exec(
				`UPDATE tags SET is_alias = 0, canonical_tag_id = NULL WHERE id = ?`, id,
			); err != nil {
				return err
			}
			logx.Warnf("import: promoted alias id=%d to a plain tag (alias cycle or missing canonical)", id)
		case root[id] != n.canonical:
			if _, err := database.Write.Exec(
				`UPDATE tags SET canonical_tag_id = ? WHERE id = ?`, root[id], id,
			); err != nil {
				return err
			}
			logx.Warnf("import: re-pointed alias id=%d through its alias chain to tag id=%d", id, root[id])
		}
	}
	return nil
}

func replaceDBFromJSON(srcPath, dbPath, thumbsPath string, b *gallery.Boundary) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open json: %w", err)
	}
	defer func() { _ = f.Close() }()
	exp, err := DecodeExport(f)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dbPath), "import-*.db")
	if err != nil {
		return fmt.Errorf("create temp db: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()
	return loadThenInstall(tmpPath, dbPath, thumbsPath, func(database *db.DB) error {
		if err := db.Bootstrap(database); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		if err := loadExportIntoDB(database, exp); err != nil {
			return err
		}
		if err := sanitizeImportedAliasChains(database); err != nil {
			return fmt.Errorf("sanitize alias chains: %w", err)
		}
		return rebaseImagePaths(database, b)
	})
}

func classifyArchive(files []*zip.File) (innerDB, innerJSON, innerLight *zip.File, gallery []*zip.File) {
	for _, f := range files {
		switch {
		case f.Name == "monbooru.db":
			innerDB = f
		case f.Name == "monbooru.json":
			innerJSON = f
		case f.Name == "tags.json":
			innerLight = f
		case strings.HasPrefix(f.Name, "gallery/") && !strings.HasSuffix(f.Name, "/"):
			gallery = append(gallery, f)
		}
	}
	return innerDB, innerJSON, innerLight, gallery
}

func replaceFromArchive(srcPath, dbPath, thumbsPath string, b *gallery.Boundary, maxFileSizeMB int) (int, error) {
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return 0, fmt.Errorf("open zip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	innerDB, innerJSON, innerLight, galleryFiles := classifyArchive(zr.File)
	if innerDB == nil && innerJSON == nil && innerLight == nil {
		if format := detectCompatFormat(zr.File); format != "" {
			return replaceFromCompatArchive(zr.File, format, dbPath, thumbsPath, b, maxFileSizeMB)
		}
		return 0, fmt.Errorf("archive missing monbooru.db, monbooru.json, or tags.json")
	}
	if innerDB == nil && innerJSON == nil {
		return replaceFromLightArchive(innerLight, galleryFiles, dbPath, thumbsPath, b, maxFileSizeMB)
	}

	// Before anything is replaced: a refused archive leaves the gallery whole.
	dsts := make([]string, len(galleryFiles))
	for i, f := range galleryFiles {
		dst, err := SafeArchiveDest(b.Root(), strings.TrimPrefix(f.Name, "gallery/"))
		if err != nil {
			return 0, fmt.Errorf("rejecting archive entry %q: %w", f.Name, err)
		}
		if !b.Excludes(dst) {
			if err := checkEntrySize(f, maxBytes); err != nil {
				return 0, err
			}
		}
		dsts[i] = dst
	}

	dataDir := filepath.Dir(dbPath)
	innerTmp, err := os.CreateTemp(dataDir, "inner-*.import")
	if err != nil {
		return 0, fmt.Errorf("create inner temp: %w", err)
	}
	innerTmpPath := innerTmp.Name()
	defer func() { _ = os.Remove(innerTmpPath) }()

	var innerFile *zip.File
	var applyInner func(string, string, string, *gallery.Boundary) error
	if innerDB != nil {
		innerFile = innerDB
		applyInner = replaceDBFromFile
	} else {
		innerFile = innerJSON
		applyInner = replaceDBFromJSON
	}
	if err := copyZipEntry(innerTmp, innerFile, maxBytes); err != nil {
		_ = innerTmp.Close()
		return 0, err
	}
	_ = innerTmp.Close()
	if err := applyInner(innerTmpPath, dbPath, thumbsPath, b); err != nil {
		return 0, err
	}

	if len(galleryFiles) == 0 {
		return 0, nil
	}

	if _, err := gallery.RemoveOwned(b); err != nil {
		return 0, fmt.Errorf("wipe gallery: %w", err)
	}
	leftOut := 0
	for i, f := range galleryFiles {
		if b.Excludes(dsts[i]) {
			leftOut++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dsts[i]), 0o755); err != nil {
			return leftOut, err
		}
		if err := copyZipFile(f, dsts[i], maxBytes); err != nil {
			return leftOut, err
		}
	}

	// applyInner reconciled before these files existed, so reconcile again.
	database, err := db.Open(dbPath)
	if err != nil {
		return leftOut, fmt.Errorf("reopen db for reconcile: %w", err)
	}
	defer func() { _ = database.Close() }()
	return leftOut, reconcileMissingFiles(database, b)
}

// Not filepath.Base: a stored path carries the writing machine's separators.
func storedBasename(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func rebaseImagePaths(database *db.DB, b *gallery.Boundary) error {
	root := strings.TrimRight(b.Root(), "/")
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Imported rows can arrive after Bootstrap's one-shot ran, so
	// normalise here: before the scan reads folder_path, and before the
	// rewrite erases the backslashes that mark a Windows row.
	if _, err := tx.Exec(db.NormalizeWindowsFolderPathSQL); err != nil {
		return fmt.Errorf("normalize folder paths: %w", err)
	}

	imgs, err := loadImagePathRows(tx)
	if err != nil {
		return fmt.Errorf("scan images for rebase: %w", err)
	}

	// The source root, inferred from the first canonical row, lets an alias
	// keep its own folder instead of collapsing into the canonical's.
	sourceRoot := ""
	for _, r := range imgs {
		if r.canonical == "" {
			continue
		}
		suffix := filepath.Join(r.folder, storedBasename(r.canonical))
		if r.folder == "" {
			suffix = storedBasename(r.canonical)
		}
		if strings.HasSuffix(r.canonical, string(filepath.Separator)+suffix) {
			sourceRoot = strings.TrimSuffix(r.canonical, string(filepath.Separator)+suffix)
			break
		}
		if r.canonical == suffix {
			sourceRoot = ""
			break
		}
	}

	for _, r := range imgs {
		newCanonical := filepath.Join(root, r.folder, storedBasename(r.canonical))
		if newCanonical == r.canonical {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE images SET canonical_path = ? WHERE id = ?`, newCanonical, r.id,
		); err != nil {
			return fmt.Errorf("update image %d: %w", r.id, err)
		}
	}

	type pathRow struct {
		id, imageID int64
		path        string
		isCanonical bool
		folder      string
	}
	paths, err := db.QueryAll(tx, func(rows *sql.Rows) (pathRow, error) {
		var r pathRow
		var isCanon int
		err := rows.Scan(&r.id, &r.imageID, &r.path, &isCanon, &r.folder)
		r.isCanonical = isCanon == 1
		return r, err
	},
		`SELECT ip.id, ip.image_id, ip.path, ip.is_canonical, i.folder_path
		 FROM image_paths ip
		 JOIN images i ON i.id = ip.image_id`)
	if err != nil {
		return fmt.Errorf("scan image_paths for rebase: %w", err)
	}

	for _, p := range paths {
		// Canonical rows rebuild exactly as images.canonical_path did.
		var newPath string
		if p.isCanonical || sourceRoot == "" {
			newPath = filepath.Join(root, p.folder, storedBasename(p.path))
		} else if rel := strings.TrimPrefix(p.path, sourceRoot+string(filepath.Separator)); rel != p.path {
			newPath = filepath.Join(root, rel)
		} else {
			newPath = filepath.Join(root, p.folder, storedBasename(p.path))
		}
		if newPath == p.path {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE image_paths SET path = ? WHERE id = ?`, newPath, p.id,
		); err != nil {
			return fmt.Errorf("update image_path %d: %w", p.id, err)
		}
	}

	if err := reconcileMissingFlags(tx, b, imgs); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return recalcImportedTagCounts(database)
}

// The export's usage counts include images the reconcile just marked
// missing; left as they are, tags claim matches the gallery can't show.
func recalcImportedTagCounts(database *db.DB) error {
	if _, err := tags.RecalcDBCount(database); err != nil {
		return fmt.Errorf("recalculate tag counts: %w", err)
	}
	return nil
}

type imgPathRow struct {
	id        int64
	folder    string
	canonical string
}

func loadImagePathRows(q db.Querier) ([]imgPathRow, error) {
	return db.QueryAll(q, func(rows *sql.Rows) (imgPathRow, error) {
		var r imgPathRow
		err := rows.Scan(&r.id, &r.folder, &r.canonical)
		return r, err
	}, `SELECT id, folder_path, canonical_path FROM images`)
}

// Both directions: an absent file is flagged, and a row exported missing
// but present here is cleared, since an import queues no Sync.
func reconcileMissingFlags(x db.Execer, b *gallery.Boundary, imgs []imgPathRow) error {
	for _, r := range imgs {
		flag := fileMissingFlag(b, r.folder, r.canonical)
		if _, err := x.Exec(`UPDATE images SET is_missing = ? WHERE id = ?`, flag, r.id); err != nil {
			return fmt.Errorf("reconcile is_missing for image %d: %w", r.id, err)
		}
	}
	return nil
}

func fileMissingFlag(b *gallery.Boundary, folder, canonical string) int {
	path := filepath.Join(b.Root(), folder, storedBasename(canonical))
	if _, err := os.Stat(path); err == nil && !b.Excludes(path) {
		return 0
	}
	return 1
}

func reconcileMissingFiles(database *db.DB, b *gallery.Boundary) error {
	imgs, err := loadImagePathRows(database.Read)
	if err != nil {
		return fmt.Errorf("scan images for reconcile: %w", err)
	}
	if err := reconcileMissingFlags(database.Write, b, imgs); err != nil {
		return err
	}
	return recalcImportedTagCounts(database)
}

func validateSQLiteFile(path string) error {
	database, err := db.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	for _, tbl := range []string{"tag_categories", "tags", "images", "image_tags"} {
		var n int
		if err := database.Read.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("missing table %q", tbl)
		}
	}
	return nil
}

func scanRow[T any](fields func(*T) []any) func(*sql.Rows) (any, error) {
	return func(rows *sql.Rows) (any, error) {
		var r T
		err := rows.Scan(fields(&r)...)
		return r, err
	}
}

type loader struct {
	tx  *sql.Tx
	err error
}

// The error names the first two arguments: the pair-keyed tables are the
// ones that collide.
func insertAll[T any](l *loader, label, query string, rows []T, args func(T) []any) {
	if l.err != nil {
		return
	}
	for _, r := range rows {
		a := args(r)
		if _, err := l.tx.Exec(query, a...); err != nil {
			l.err = fmt.Errorf("insert %s %v: %w", label, a[:min(2, len(a))], err)
			return
		}
	}
}

func loadExportIntoDB(database *db.DB, exp Export) error {
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Deferred: an alias created before its canonical has the lower id.
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return fmt.Errorf("defer fk: %w", err)
	}

	// Clears what Bootstrap seeded, which would collide with the imported ids.
	for _, stmt := range []string{
		`DELETE FROM image_tags`,
		`DELETE FROM image_tag_sources`,
		`DELETE FROM tag_implications`,
		`DELETE FROM image_paths`,
		`DELETE FROM image_collections`,
		`DELETE FROM image_sources`,
		`DELETE FROM image_annotations`,
		`DELETE FROM sd_metadata`,
		`DELETE FROM comfyui_metadata`,
		`DELETE FROM manga_metadata`,
		`DELETE FROM saved_searches`,
		`DELETE FROM dup_group_members`,
		`DELETE FROM dup_groups`,
		`DELETE FROM alt_group_members`,
		`DELETE FROM alt_groups`,
		`DELETE FROM version_edges`,
		`DELETE FROM derivative_edges`,
		`DELETE FROM not_related_pairs`,
		`DELETE FROM images`,
		`DELETE FROM tags`,
		`DELETE FROM tag_categories`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("reset table: %w", err)
		}
	}
	l := &loader{tx: tx}

	for _, r := range exp.TagCategories {
		// The color lands in an inline style attribute: don't trust it.
		safeColor := tags.SafeCategoryColor(r.Color)
		if safeColor != r.Color {
			logx.Warnf("import: replaced invalid color %q for tag_category %q with %s", r.Color, r.Name, safeColor)
		}
		if _, err := tx.Exec(
			`INSERT INTO tag_categories (id, name, color, is_builtin) VALUES (?, ?, ?, ?)`,
			r.ID, r.Name, safeColor, r.IsBuiltin,
		); err != nil {
			return fmt.Errorf("insert tag_category %q: %w", r.Name, err)
		}
	}
	insertAll(l, "tag",
		`INSERT INTO tags (id, name, category_id, usage_count, is_alias, canonical_tag_id, created_at, origin, last_used_at, stale)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exp.Tags, func(r TagRow) []any {
			return []any{r.ID, r.Name, r.CategoryID, r.UsageCount, r.IsAlias, r.CanonicalTagID, r.CreatedAt, r.Origin, r.LastUsedAt, r.Stale}
		})
	insertAll(l, "tag_implication",
		`INSERT INTO tag_implications (parent_tag_id, implied_tag_id, created_at, origin, stale) VALUES (?, ?, ?, ?, ?)`,
		exp.TagImplications, func(r TagImplicationRow) []any {
			return []any{r.ParentTagID, r.ImpliedTagID, r.CreatedAt, r.Origin, r.Stale}
		})
	insertAll(l, "tag_note",
		`INSERT INTO tag_notes (tag_id, body, links) VALUES (?, ?, ?)`,
		exp.TagNotes, func(r TagNoteRow) []any { return []any{r.TagID, r.Body, r.Links} })
	// Pre-v10 documents lack the opt-ins, which would decode as 0: opted out.
	preV10 := exp.Version < 10
	insertAll(l, "image",
		`INSERT INTO images (id, sha256, md5, canonical_path, folder_path, file_type, width, height,
		                    file_size, is_missing, is_favorited, is_inbox, auto_tagged_at, source_type, origin, source, url, page_count, duration_seconds, series, series_order, note, original_source,
		                    phash, last_read_page, upload_batch, scheduled_lookup, scheduled_lookup_ptr, ingested_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exp.Images, func(r ImageRow) []any {
			scheduled, scheduledPTR := r.ScheduledLookup, r.ScheduledLookupPTR
			if preV10 {
				scheduled, scheduledPTR = 1, 1
			}
			return []any{r.ID, r.SHA256, r.MD5, r.CanonicalPath, r.FolderPath, r.FileType, r.Width, r.Height,
				r.FileSize, r.IsMissing, r.IsFavorited, r.IsInbox, r.AutoTaggedAt, r.SourceType, r.Origin, r.Source, r.URL, r.PageCount, r.DurationSeconds, r.Series, r.SeriesOrder, r.Note, r.OriginalSource,
				r.Phash, r.LastReadPage, r.UploadBatch, scheduled, scheduledPTR, r.IngestedAt}
		})
	if exp.Version < 3 {
		// Pre-v3 documents hold collections only in images.series.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_collections (image_id, name, position)
			 SELECT id, series, series_order FROM images WHERE series != ''`,
		); err != nil {
			return fmt.Errorf("seed image_collections: %w", err)
		}
	}
	insertAll(l, "image_collection",
		`INSERT OR IGNORE INTO image_collections (image_id, name, position) VALUES (?, ?, ?)`,
		exp.ImageCollections, func(r ImageCollectionRow) []any {
			return []any{r.ImageID, r.Name, r.Position}
		})
	insertAll(l, "collection_find_relations",
		`INSERT OR IGNORE INTO collection_find_relations (name) VALUES (?)`,
		exp.FindRelations, func(r FindRelationsRow) []any { return []any{r.Name} })
	insertAll(l, "image_source",
		`INSERT OR IGNORE INTO image_sources (image_id, site, post_id, url, md5, commentary, commentary_translated, original, similarity,
		                                     md5_match, parent_url, upgrade_kept, post_width, post_height, post_size, post_ext, fetched_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exp.ImageSources, func(r ImageSourceRow) []any {
			return []any{r.ImageID, r.Site, r.PostID, r.URL, r.MD5, r.Commentary, r.Translated, r.Original, r.Similarity,
				r.MD5Match, r.ParentURL, r.UpgradeKept, r.PostWidth, r.PostHeight, r.PostSize, r.PostExt, r.FetchedAt}
		})
	insertAll(l, "image_annotation (image)",
		`INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body, manual, fetched_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exp.ImageAnnotations, func(r ImageAnnotationRow) []any {
			return []any{r.ImageID, r.Site, r.PostID, r.X, r.Y, r.W, r.H, r.Body, r.Manual, r.FetchedAt}
		})
	insertAll(l, "image_path",
		`INSERT INTO image_paths (id, image_id, path, is_canonical, mtime_unix, mtime_nsec) VALUES (?, ?, ?, ?, ?, ?)`,
		exp.ImagePaths, func(r ImagePathRow) []any {
			return []any{r.ID, r.ImageID, r.Path, r.IsCanonical, r.MtimeUnix, r.MtimeNsec}
		})
	for _, r := range exp.ImageTags {
		var conf, tname any
		if r.Confidence.Valid {
			conf = r.Confidence.Float64
		}
		if r.TaggerName.Valid {
			tname = r.TaggerName.String
		}
		if _, err := tx.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name, created_at, stale)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ImageID, r.TagID, r.IsAuto, r.IsImplied, conf, tname, r.CreatedAt, r.Stale,
		); err != nil {
			return fmt.Errorf("insert image_tag (%d,%d): %w", r.ImageID, r.TagID, err)
		}
	}
	insertAll(l, "image_tag_source",
		`INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source, created_at) VALUES (?, ?, ?, ?)`,
		exp.ImageTagSources, func(r ImageTagSourceRow) []any {
			return []any{r.ImageID, r.TagID, r.Source, r.CreatedAt}
		})
	if exp.Version < 11 {
		// Pre-v11 documents carry no ledger, so derive one.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source, created_at)
			 SELECT image_id, tag_id, COALESCE(NULLIF(tagger_name, ''), 'user'), created_at
			 FROM image_tags WHERE is_implied = 0`,
		); err != nil {
			return fmt.Errorf("backfill image_tag_sources: %w", err)
		}
	}
	insertAll(l, "sd_metadata",
		`INSERT INTO sd_metadata (image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exp.SDMetadata, func(r SDMetadataRow) []any {
			return []any{r.ImageID, r.Prompt, r.NegativePrompt, r.Model,
				r.Seed, r.Sampler, r.Steps,
				r.CFGScale, r.RawParams, r.GenerationHash}
		})
	insertAll(l, "comfyui_metadata",
		`INSERT INTO comfyui_metadata (image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exp.ComfyUIMetadata, func(r ComfyMetadataRow) []any {
			return []any{r.ImageID, r.Prompt, r.ModelCheckpoint,
				r.Seed, r.Sampler, r.Steps,
				r.CFGScale, r.RawWorkflow, r.GenerationHash}
		})
	insertAll(l, "manga_metadata",
		`INSERT INTO manga_metadata (image_id, title, series, number, volume, count, summary, notes,
		     year, month, day, writer, penciller, inker, colorist, letterer, cover_artist, editor, publisher,
		     imprint, genre, web, language_iso, format, manga, age_rating, community_rating, xml_page_count, raw_xml)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exp.MangaMetadata, func(r MangaMetadataRow) []any {
			return []any{r.ImageID, r.Title, r.Series, r.Number, r.Volume,
				r.Count, r.Summary, r.Notes,
				r.Year, r.Month, r.Day,
				r.Writer, r.Penciller, r.Inker, r.Colorist,
				r.Letterer, r.CoverArtist, r.Editor, r.Publisher,
				r.Imprint, r.Genre, r.Web, r.LanguageISO,
				r.Format, r.Manga, r.AgeRating,
				r.CommunityRating, r.XMLPageCount, r.RawXML}
		})
	insertAll(l, "dup_group",
		`INSERT INTO dup_groups (id, original_image_id, created_at) VALUES (?, ?, ?)`,
		exp.DupGroups, func(r DupGroupRow) []any {
			return []any{r.ID, r.OriginalImageID, r.CreatedAt}
		})
	insertAll(l, "dup_group_member",
		`INSERT INTO dup_group_members (image_id, group_id, created_at) VALUES (?, ?, ?)`,
		exp.DupGroupMembers, func(r DupGroupMemberRow) []any {
			return []any{r.ImageID, r.GroupID, r.CreatedAt}
		})
	insertAll(l, "alt_group",
		`INSERT INTO alt_groups (id, created_at) VALUES (?, ?)`,
		exp.AltGroups, func(r AltGroupRow) []any {
			return []any{r.ID, r.CreatedAt}
		})
	insertAll(l, "alt_group_member",
		`INSERT INTO alt_group_members (image_id, group_id, created_at) VALUES (?, ?, ?)`,
		exp.AltGroupMembers, func(r AltGroupMemberRow) []any {
			return []any{r.ImageID, r.GroupID, r.CreatedAt}
		})
	insertAll(l, "version_edge",
		`INSERT INTO version_edges (child_image_id, parent_image_id, created_at) VALUES (?, ?, ?)`,
		exp.VersionEdges, func(r VersionEdgeRow) []any {
			return []any{r.ChildImageID, r.ParentImageID, r.CreatedAt}
		})
	insertAll(l, "derivative_edge",
		`INSERT INTO derivative_edges (derivative_image_id, source_image_id, created_at) VALUES (?, ?, ?)`,
		exp.DerivativeEdges, func(r DerivativeEdgeRow) []any {
			return []any{r.DerivativeImageID, r.SourceImageID, r.CreatedAt}
		})
	// The relations service only matches a < b, which an older or
	// hand-written document may not follow; OR IGNORE for one that names
	// both orientations.
	insertAll(l, "not_related_pair",
		`INSERT OR IGNORE INTO not_related_pairs (a_image_id, b_image_id, created_at) VALUES (?, ?, ?)`,
		exp.NotRelatedPairs, func(r NotRelatedPairRow) []any {
			return []any{min(r.AImageID, r.BImageID), max(r.AImageID, r.BImageID), r.CreatedAt}
		})
	insertAll(l, "saved_search",
		`INSERT INTO saved_searches (id, name, query, sort, sort_order, seed, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		exp.SavedSearches, func(r SavedSearchRow) []any {
			return []any{r.ID, r.Name, r.Query, r.Sort, r.Order, r.Seed, r.CreatedAt}
		})
	insertAll(l, "image_lookup",
		`INSERT INTO image_lookups (image_id, backend, attempts, queued_at, job_id, last_at, last_result, next_due_at, ptr_cursor)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exp.ImageLookups, func(r ImageLookupRow) []any {
			return []any{r.ImageID, r.Backend, r.Attempts, r.QueuedAt, r.JobID, r.LastAt, r.LastResult, r.NextDueAt, r.PTRCursor}
		})
	if l.err != nil {
		return l.err
	}
	return tx.Commit()
}

// jsonWriter streams so an export holds one row in memory, never a whole table.
type jsonWriter struct {
	w     io.Writer
	err   error
	first bool
}

func newJSONWriter(w io.Writer) *jsonWriter { return &jsonWriter{w: w} }

func (j *jsonWriter) writeStr(s string) {
	if j.err != nil {
		return
	}
	_, j.err = j.w.Write([]byte(s))
}

func (j *jsonWriter) raw(b []byte) {
	if j.err != nil {
		return
	}
	_, j.err = j.w.Write(b)
}

func (j *jsonWriter) objStart() {
	j.writeStr("{")
	j.first = true
}

func (j *jsonWriter) objEnd() { j.writeStr("}\n") }

func (j *jsonWriter) comma() {
	if j.first {
		j.first = false
	} else {
		j.writeStr(",")
	}
}

func (j *jsonWriter) field(name string, value any) {
	j.comma()
	j.marshalAndWrite(name)
	j.writeStr(":")
	j.marshalAndWrite(value)
}

func (j *jsonWriter) arrayStart(name string) {
	j.comma()
	j.marshalAndWrite(name)
	j.writeStr(":[")
}

func (j *jsonWriter) arrayEnd() { j.writeStr("]") }

func (j *jsonWriter) arrayItem(first *bool, value any) {
	if !*first {
		j.writeStr(",")
	}
	*first = false
	j.marshalAndWrite(value)
}

func (j *jsonWriter) marshalAndWrite(value any) {
	if j.err != nil {
		return
	}
	b, err := json.Marshal(value)
	if err != nil {
		j.err = err
		return
	}
	j.raw(b)
}

func streamRows(j *jsonWriter, key string, q db.Querier, query string, scan func(*sql.Rows) (any, error), args ...any) {
	if j.err != nil {
		return
	}
	j.arrayStart(key)
	first := true
	rows, err := q.Query(query, args...)
	if err != nil {
		j.arrayEnd()
		j.err = err
		return
	}
	defer func() { _ = rows.Close() }()
	defer j.arrayEnd()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			j.err = err
			return
		}
		j.arrayItem(&first, v)
		if j.err != nil {
			return
		}
	}
	if err := rows.Err(); err != nil && j.err == nil {
		j.err = err
	}
}

func FormatFromExt(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".db", ".sqlite":
		return "db"
	case ".json":
		return "json"
	case ".zip":
		return "zip"
	}
	return ""
}
