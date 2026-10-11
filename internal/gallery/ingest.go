package gallery

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
)

// FolderPath returns "" for the root and outside it; the value is stored,
// so it is "/"-separated on every platform.
func FolderPath(galleryPath, filePath string) string {
	// Rel cleans both sides, so a "/"-configured gallery path still
	// matches native walk paths.
	rel, err := filepath.Rel(galleryPath, filepath.Dir(filePath))
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// Ingest's bool reports a duplicate of an existing image; an empty origin
// records OriginIngest.
func Ingest(h Handle, path, origin string) (*models.Image, bool, error) {
	img, out, err := ingestPath(h, path, origin)
	return img, out == ingestDuplicate, err
}

type ingestOutcome int

const (
	ingestCreated ingestOutcome = iota
	ingestDuplicate
	ingestMoved
	ingestRestored
	ingestEdited
)

func ingestPath(h Handle, path, origin string) (*models.Image, ingestOutcome, error) {
	hash, sum, err := hashFileDigests(path)
	if err != nil {
		return nil, ingestCreated, fmt.Errorf("hashing file: %w", err)
	}
	claimOwnership(h.GalleryPath, path)
	return ingestWithHash(h, path, hash, sum, origin)
}

func decodeImageDimensions(path string) (w, h *int) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = f.Close() }()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, nil
	}
	return &cfg.Width, &cfg.Height
}

func stillDimensions(path, fileType string) (w, h *int) {
	if !IsFFmpegStill(fileType) {
		return decodeImageDimensions(path)
	}
	pw, ph, ok := ProbeVideoDimensions(path)
	if !ok {
		return nil, nil
	}
	return &pw, &ph
}

// The caller supplies path's digests and claims its ownership.
func ingestWithHash(h Handle, path, hash, sum, origin string) (*models.Image, ingestOutcome, error) {
	database, galleryPath, thumbnailsPath := h.DB, h.GalleryPath, h.ThumbnailsPath
	origin = cmp.Or(origin, models.OriginIngest)

	// The path's own row decides first: by SHA alone, a file renamed over
	// another would read as a move of that other image onto this path.
	var prevID int64
	var prevCanonical int
	var prevSHA string
	switch pathErr := database.Read.QueryRow(
		`SELECT ip.image_id, ip.is_canonical, i.sha256 FROM image_paths ip JOIN images i ON i.id = ip.image_id WHERE ip.path = ?`, path,
	).Scan(&prevID, &prevCanonical, &prevSHA); {
	case pathErr == sql.ErrNoRows, pathErr == nil && prevSHA == hash:
	case pathErr != nil:
		return nil, ingestCreated, fmt.Errorf("checking image_paths: %w", pathErr)
	case prevCanonical == 1:
		return ingestInPlaceEdit(h, path, hash, sum, prevID)
	default:
		// A rewritten alias: the image keeps its own file, and this path
		// is classified afresh.
		if _, delErr := database.Write.Exec(`DELETE FROM image_paths WHERE path = ? AND is_canonical = 0`, path); delErr != nil {
			return nil, ingestCreated, fmt.Errorf("dropping stale alias path: %w", delErr)
		}
	}

	existingID, err := ImageIDBySHA(database.Read, hash)

	if err == nil {
		var img models.Image
		var isMissingInt int
		scanErr := database.Read.QueryRow(
			`SELECT id, sha256, canonical_path, folder_path, file_type, file_size, is_missing FROM images WHERE id = ?`,
			existingID,
		).Scan(&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType, &img.FileSize, &isMissingInt)
		if scanErr != nil {
			return nil, ingestDuplicate, fmt.Errorf("looking up duplicate image %d: %w", existingID, scanErr)
		}
		img.IsMissing = isMissingInt == 1

		if img.IsMissing {
			newFolder := FolderPath(galleryPath, path)
			tx, txErr := database.Write.Begin()
			if txErr != nil {
				return nil, ingestCreated, fmt.Errorf("begin reactivation tx: %w", txErr)
			}
			defer func() { _ = tx.Rollback() }()
			if err := repointCanonical(tx, existingID, path, newFolder, ""); err != nil {
				return nil, ingestCreated, fmt.Errorf("reactivate image: %w", err)
			}
			if _, err := tx.Exec(`UPDATE images SET md5 = ? WHERE id = ?`, sum, existingID); err != nil {
				return nil, ingestCreated, fmt.Errorf("reactivate image md5: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, ingestCreated, fmt.Errorf("commit reactivation: %w", err)
			}
			_ = generate(path, thumbnailsPath, existingID, img.FileType)
			img.IsMissing = false
			img.CanonicalPath = path
			img.FolderPath = newFolder
			logx.Infof("ingest: reactivated previously missing image id=%d path=%q", existingID, path)
			return &img, ingestRestored, nil
		}

		// The old canonical is gone, so this is a move: the old path kept
		// as an alias would show as a phantom duplicate.
		if _, statErr := os.Stat(img.CanonicalPath); statErr != nil {
			newFolder := FolderPath(galleryPath, path)
			tx, txErr := database.Write.Begin()
			if txErr != nil {
				return nil, ingestCreated, fmt.Errorf("begin promote tx: %w", txErr)
			}
			defer func() { _ = tx.Rollback() }()
			if err := repointCanonical(tx, existingID, path, newFolder, img.CanonicalPath); err != nil {
				return nil, ingestCreated, fmt.Errorf("promote canonical path: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, ingestCreated, fmt.Errorf("commit promote: %w", err)
			}
			img.CanonicalPath = path
			img.FolderPath = newFolder
			logx.Infof("ingest: promoted alias to canonical for image id=%d (old path gone) %q", existingID, path)
			return &img, ingestMoved, nil
		}

		_, aliasErr := database.Write.Exec(
			`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
			existingID, path,
		)
		if aliasErr != nil {
			logx.Warnf("ingest alias: %v", aliasErr)
		}
		return &img, ingestDuplicate, nil
	}
	if err != sql.ErrNoRows {
		return nil, ingestCreated, fmt.Errorf("checking sha256: %w", err)
	}

	// The walk trusts extensions, so the signature decides here whether
	// to add a row and which type it records.
	fileType, err := detectMagicType(path)
	if err != nil {
		logx.Warnf("ingest: skip %q: contents are not a supported media type", path)
		return nil, ingestCreated, fmt.Errorf("ingest %q: %w", path, err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		return nil, ingestCreated, fmt.Errorf("stat file: %w", err)
	}

	folderPath := FolderPath(galleryPath, path)

	var imgWidth, imgHeight *int
	var pageCount *int
	var durationSec *float64
	var prefilledSeries string
	var mangaMeta *models.MangaMetadata
	if IsVideoType(fileType) {
		if d, ok := ProbeDurationSeconds(path); ok {
			durationSec = &d
		}
		if w, h, ok := ProbeVideoDimensions(path); ok {
			imgWidth, imgHeight = &w, &h
		}
	}
	if fileType == models.FileTypeCBZ {
		archive, openErr := OpenManga(path)
		if openErr != nil {
			logx.Warnf("ingest: skip manga %q: %v", path, openErr)
			return nil, ingestCreated, fmt.Errorf("ingest manga: %w", openErr)
		}
		w, h, dimErr := archive.coverDimensions()
		if dimErr == nil {
			imgWidth, imgHeight = &w, &h
		} else {
			logx.Warnf("ingest manga %q: cover dimensions: %v", path, dimErr)
		}
		mm, mmErr := metadata.ParseComicInfo(archive.Reader())
		if mmErr != nil {
			logx.Warnf("ingest manga %q: ComicInfo: %v", path, mmErr)
		} else if mm != nil {
			mangaMeta = mm
			prefilledSeries = mm.Series
		}
		pcVal := len(archive.Pages)
		pageCount = &pcVal
		_ = archive.Close()
	} else if !IsVideoType(fileType) {
		imgWidth, imgHeight = stillDimensions(path, fileType)
	}

	sdMeta, comfyMeta, sourceType := extractGenerationMeta(path, fileType)

	// ON CONFLICT DO NOTHING: a concurrent ingest may have written this
	// SHA since the read-pool check.
	tx, err := database.Write.Begin()
	if err != nil {
		return nil, ingestCreated, err
	}
	defer func() { _ = tx.Rollback() }()

	var imgID int64
	insertErr := tx.QueryRow(
		`INSERT INTO images (sha256, md5, canonical_path, folder_path, file_type, width, height, file_size, source_type, origin, page_count, series, duration_seconds)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(sha256) DO NOTHING
		 RETURNING id`,
		hash, sum, path, folderPath, fileType, imgWidth, imgHeight, fi.Size(), sourceType, origin, pageCount, prefilledSeries, durationSec,
	).Scan(&imgID)

	if insertErr == sql.ErrNoRows {
		existingID, err := ImageIDBySHA(tx, hash)
		if err != nil {
			return nil, ingestCreated, fmt.Errorf("race: fetch existing sha: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
			existingID, path,
		); err != nil {
			return nil, ingestCreated, fmt.Errorf("race: insert alias path: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, ingestCreated, fmt.Errorf("race: commit alias: %w", err)
		}
		var img models.Image
		var isMissingInt int
		if err := database.Read.QueryRow(
			`SELECT id, sha256, canonical_path, folder_path, file_type, file_size, is_missing FROM images WHERE id = ?`,
			existingID,
		).Scan(&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType, &img.FileSize, &isMissingInt); err != nil {
			return nil, ingestDuplicate, fmt.Errorf("race: reload existing image %d: %w", existingID, err)
		}
		img.IsMissing = isMissingInt == 1
		return &img, ingestDuplicate, nil
	}
	if insertErr != nil {
		return nil, ingestCreated, fmt.Errorf("inserting image: %w", insertErr)
	}

	if _, err := tx.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical, mtime_unix, mtime_nsec) VALUES (?, ?, 1, ?, ?)`,
		imgID, path, fi.ModTime().Unix(), fi.ModTime().UnixNano(),
	); err != nil {
		return nil, ingestCreated, fmt.Errorf("inserting image_path: %w", err)
	}

	if err := insertGenerationMetadata(context.Background(), tx, imgID, sdMeta, comfyMeta); err != nil {
		return nil, ingestCreated, err
	}
	if mangaMeta != nil {
		mangaMeta.ImageID = imgID
		if err := insertMangaMeta(tx, mangaMeta); err != nil {
			return nil, ingestCreated, fmt.Errorf("inserting manga_metadata: %w", err)
		}
	}
	if prefilledSeries != "" {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_collections (image_id, name, position) VALUES (?, ?, NULL)`,
			imgID, prefilledSeries,
		); err != nil {
			return nil, ingestCreated, fmt.Errorf("inserting image_collection: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, ingestCreated, fmt.Errorf("committing ingest: %w", err)
	}

	phash := regenerateDerived(database, thumbnailsPath, path, imgID, fileType, "ingest")
	// After the thumbnail, which is what the greyscale probe reads.
	if err := ApplyMetaTags(h, imgID); err != nil {
		logx.Warnf("ingest: meta tags for %q: %v", path, err)
	}

	img := &models.Image{
		Phash:         phash,
		ID:            imgID,
		SHA256:        hash,
		CanonicalPath: path,
		FolderPath:    folderPath,
		FileType:      fileType,
		Width:         imgWidth,
		Height:        imgHeight,
		FileSize:      fi.Size(),
		SourceType:    sourceType,
		Origin:        origin,
		PageCount:     pageCount,
		DurationSec:   durationSec,
		Series:        prefilledSeries,
		IngestedAt:    time.Now().UTC(),
	}
	return img, ingestCreated, nil
}

var ErrRewritten = errors.New("was rewritten with the bytes of image")

// images.sha256 is UNIQUE, and which row the operator meant when a file
// takes another image's bytes is not ours to guess.
func ingestInPlaceEdit(h Handle, path, hash, sum string, id int64) (*models.Image, ingestOutcome, error) {
	var otherID int64
	var otherPath string
	switch err := h.DB.Read.QueryRow(
		`SELECT id, canonical_path FROM images WHERE sha256 = ?`, hash,
	).Scan(&otherID, &otherPath); {
	case err == nil:
		return nil, ingestCreated, fmt.Errorf("%q %w %d at %q; row left as it is", path, ErrRewritten, otherID, otherPath)
	case err != sql.ErrNoRows:
		return nil, ingestCreated, fmt.Errorf("checking sha256: %w", err)
	}
	fileType, err := detectMagicType(path)
	if err != nil {
		logx.Warnf("ingest: skip %q: contents are not a supported media type", path)
		return nil, ingestCreated, fmt.Errorf("ingest %q: %w", path, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, ingestCreated, fmt.Errorf("stat file: %w", err)
	}
	phash, err := applyInPlaceEdit(h, path, hash, sum, fi.ModTime().Unix(), fi.ModTime().UnixNano(), fi.Size())
	if err != nil {
		return nil, ingestCreated, err
	}
	return &models.Image{
		ID: id, SHA256: hash, MD5: sum, CanonicalPath: path, FolderPath: FolderPath(h.GalleryPath, path),
		FileType: fileType, FileSize: fi.Size(), Phash: phash,
	}, ingestEdited, nil
}

// A path that rewrites a file's bytes must store all three.
func extractGenerationMeta(path, fileType string) (*models.SDMetadata, *models.ComfyUIMetadata, string) {
	sd, comfy, _ := metadata.Extract(path, fileType)
	switch {
	case sd != nil && comfy != nil:
		return sd, comfy, models.SourceTypeBoth
	case sd != nil:
		return sd, comfy, models.SourceTypeA1111
	case comfy != nil:
		return sd, comfy, models.SourceTypeComfyUI
	}
	return sd, comfy, models.SourceTypeNone
}

// replaceGenerationMetadata drops the old rows even when no new ones
// arrive: a rewrite can strip a recipe.
func replaceGenerationMetadata(ctx context.Context, tx *sql.Tx, imageID int64, sd *models.SDMetadata, comfy *models.ComfyUIMetadata) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sd_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("clear sd_metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM comfyui_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("clear comfyui_metadata: %w", err)
	}
	return insertGenerationMetadata(ctx, tx, imageID, sd, comfy)
}

func insertGenerationMetadata(ctx context.Context, tx *sql.Tx, imageID int64, sd *models.SDMetadata, comfy *models.ComfyUIMetadata) error {
	if sd != nil {
		sd.ImageID = imageID
		if err := insertSDMeta(ctx, tx, sd); err != nil {
			return fmt.Errorf("insert sd_metadata: %w", err)
		}
	}
	if comfy != nil {
		comfy.ImageID = imageID
		if err := insertComfyMeta(ctx, tx, comfy); err != nil {
			return fmt.Errorf("insert comfyui_metadata: %w", err)
		}
	}
	return nil
}

func insertSDMeta(ctx context.Context, tx *sql.Tx, sd *models.SDMetadata) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO sd_metadata (image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sd.ImageID, sd.Prompt, sd.NegativePrompt, sd.Model, sd.Seed, sd.Sampler, sd.Steps, sd.CFGScale, sd.RawParams, sd.GenerationHash,
	)
	return err
}

func insertComfyMeta(ctx context.Context, tx *sql.Tx, comfy *models.ComfyUIMetadata) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO comfyui_metadata (image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		comfy.ImageID, comfy.Prompt, comfy.ModelCheckpoint, comfy.Seed, comfy.Sampler, comfy.Steps, comfy.CFGScale, comfy.RawWorkflow, comfy.GenerationHash,
	); err != nil {
		return err
	}
	return writeComfyTerms(ctx, tx, comfy.ImageID, comfy.RawWorkflow)
}

func insertMangaMeta(tx *sql.Tx, m *models.MangaMetadata) error {
	_, err := tx.Exec(
		`INSERT OR REPLACE INTO manga_metadata (image_id, title, series, number, volume, count, summary, notes,
		     year, month, day, writer, penciller, inker, colorist, letterer, cover_artist, editor, publisher,
		     imprint, genre, web, language_iso, format, manga, age_rating, community_rating, xml_page_count, raw_xml)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?,
		         ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		         ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ImageID, m.Title, m.Series, m.Number, m.Volume,
		m.Count, m.Summary, m.Notes,
		m.Year, m.Month, m.Day,
		m.Writer, m.Penciller, m.Inker, m.Colorist, m.Letterer, m.CoverArtist, m.Editor, m.Publisher,
		m.Imprint, m.Genre, m.Web, m.LanguageISO, m.Format, m.Manga, m.AgeRating,
		m.CommunityRating, m.XMLPageCount, m.RawXML,
	)
	return err
}

// DropDuplicateCopy leaves the canonical path alone: an ingest of bytes
// rewritten there also reports a duplicate, and that file is the only copy.
func DropDuplicateCopy(database *db.DB, imageID int64, path, logCtx string) {
	var canonical string
	if err := database.Read.QueryRow(
		`SELECT canonical_path FROM images WHERE id = ?`, imageID,
	).Scan(&canonical); err != nil {
		logx.Warnf("%s: canonical path for image %d: %v", logCtx, imageID, err)
		return
	}
	if canonical == path {
		return
	}
	// With the canonical gone from disk this copy is the only one, and a sync promotes it.
	if _, err := os.Stat(canonical); err != nil {
		return
	}
	if _, err := database.Write.Exec(
		`DELETE FROM image_paths WHERE image_id = ? AND path = ? AND is_canonical = 0`,
		imageID, path,
	); err != nil {
		logx.Warnf("%s: drop duplicate alias for %q: %v", logCtx, path, err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logx.Warnf("%s: remove duplicate copy %q: %v", logCtx, path, err)
	}
}
