package gallery

import (
	"cmp"
	"context"
	"database/sql"
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
func Ingest(database *db.DB, galleryPath, thumbnailsPath, path, origin string) (*models.Image, bool, error) {
	hash, sum, err := hashFileDigests(path)
	if err != nil {
		return nil, false, fmt.Errorf("hashing file: %w", err)
	}
	claimOwnership(galleryPath, path)
	return ingestWithHash(database, galleryPath, thumbnailsPath, path, hash, sum, origin)
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
func ingestWithHash(database *db.DB, galleryPath, thumbnailsPath, path, hash, sum, origin string) (*models.Image, bool, error) {
	origin = cmp.Or(origin, models.OriginIngest)
	var existingID int64
	err := database.Read.QueryRow(
		`SELECT id FROM images WHERE sha256 = ?`, hash,
	).Scan(&existingID)

	if err == nil {
		var img models.Image
		var isMissingInt int
		scanErr := database.Read.QueryRow(
			`SELECT id, sha256, canonical_path, folder_path, file_type, file_size, is_missing FROM images WHERE id = ?`,
			existingID,
		).Scan(&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType, &img.FileSize, &isMissingInt)
		if scanErr != nil {
			return nil, true, fmt.Errorf("looking up duplicate image %d: %w", existingID, scanErr)
		}
		img.IsMissing = isMissingInt == 1

		if img.IsMissing {
			newFolder := FolderPath(galleryPath, path)
			tx, txErr := database.Write.Begin()
			if txErr != nil {
				return nil, false, fmt.Errorf("begin reactivation tx: %w", txErr)
			}
			defer func() { _ = tx.Rollback() }()
			if err := repointCanonical(tx, existingID, path, newFolder, ""); err != nil {
				return nil, false, fmt.Errorf("reactivate image: %w", err)
			}
			if _, err := tx.Exec(`UPDATE images SET md5 = ? WHERE id = ?`, sum, existingID); err != nil {
				return nil, false, fmt.Errorf("reactivate image md5: %w", err)
			}
			// usage_count counts visible images, so every tag still
			// attached gets back the slot the missing mark took.
			if _, err := tx.Exec(
				`UPDATE tags SET usage_count = usage_count + 1
				 WHERE id IN (SELECT tag_id FROM image_tags WHERE image_id = ?)`,
				existingID,
			); err != nil {
				return nil, false, fmt.Errorf("restore usage counts: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, false, fmt.Errorf("commit reactivation: %w", err)
			}
			_ = Generate(path, thumbnailsPath, existingID, img.FileType)
			img.IsMissing = false
			img.CanonicalPath = path
			img.FolderPath = newFolder
			logx.Infof("ingest: reactivated previously missing image id=%d path=%q", existingID, path)
			return &img, false, nil
		}

		// The old canonical is gone, so this is a move: the old path kept
		// as an alias would show as a phantom duplicate.
		if _, statErr := os.Stat(img.CanonicalPath); statErr != nil {
			newFolder := FolderPath(galleryPath, path)
			tx, txErr := database.Write.Begin()
			if txErr != nil {
				return nil, false, fmt.Errorf("begin promote tx: %w", txErr)
			}
			defer func() { _ = tx.Rollback() }()
			if err := repointCanonical(tx, existingID, path, newFolder, img.CanonicalPath); err != nil {
				return nil, false, fmt.Errorf("promote canonical path: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, false, fmt.Errorf("commit promote: %w", err)
			}
			img.CanonicalPath = path
			img.FolderPath = newFolder
			logx.Infof("ingest: promoted alias to canonical for image id=%d (old path gone) %q", existingID, path)
			return &img, false, nil
		}

		_, aliasErr := database.Write.Exec(
			`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
			existingID, path,
		)
		if aliasErr != nil {
			logx.Warnf("ingest alias: %v", aliasErr)
		}
		return &img, true, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, fmt.Errorf("checking sha256: %w", err)
	}

	// The walk trusts extensions, so the signature decides here whether
	// to add a row and which type it records.
	fileType, err := detectMagicType(path)
	if err != nil {
		logx.Warnf("ingest: skip %q: contents are not a supported media type", path)
		return nil, false, fmt.Errorf("ingest %q: %w", path, err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		return nil, false, fmt.Errorf("stat file: %w", err)
	}

	folderPath := FolderPath(galleryPath, path)

	// A path registered under another SHA was rewritten in place;
	// image_paths.path is UNIQUE, so the insert below would fail.
	var prevID int64
	var prevCanonical int
	switch pathErr := database.Read.QueryRow(
		`SELECT image_id, is_canonical FROM image_paths WHERE path = ?`, path,
	).Scan(&prevID, &prevCanonical); {
	case pathErr == nil && prevCanonical == 1:
		phash, editErr := applyInPlaceEdit(database, thumbnailsPath, path, hash, sum,
			fi.ModTime().Unix(), fi.ModTime().UnixNano(), fi.Size())
		if editErr != nil {
			return nil, false, editErr
		}
		return &models.Image{
			ID: prevID, SHA256: hash, MD5: sum, CanonicalPath: path, FolderPath: folderPath,
			FileType: fileType, FileSize: fi.Size(), Phash: phash,
		}, false, nil
	case pathErr == nil:
		// A rewritten alias: the image keeps its own file, and this path
		// becomes a new image.
		if _, delErr := database.Write.Exec(`DELETE FROM image_paths WHERE path = ?`, path); delErr != nil {
			return nil, false, fmt.Errorf("dropping stale alias path: %w", delErr)
		}
	case pathErr != sql.ErrNoRows:
		return nil, false, fmt.Errorf("checking image_paths: %w", pathErr)
	}

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
			return nil, false, fmt.Errorf("ingest manga: %w", openErr)
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
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var imgID int64
	insertErr := tx.QueryRow(
		`INSERT INTO images (sha256, md5, canonical_path, folder_path, file_type, width, height, file_size, source_type, origin, page_count, series, duration_seconds)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(sha256) DO NOTHING
		 RETURNING id`,
		hash, sum, path, folderPath, fileType, toNullInt(imgWidth), toNullInt(imgHeight), fi.Size(), sourceType, origin, toNullInt(pageCount), prefilledSeries, toNullFloat(durationSec),
	).Scan(&imgID)

	if insertErr == sql.ErrNoRows {
		var existingID int64
		if err := tx.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, hash).Scan(&existingID); err != nil {
			return nil, false, fmt.Errorf("race: fetch existing sha: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
			existingID, path,
		); err != nil {
			return nil, false, fmt.Errorf("race: insert alias path: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, false, fmt.Errorf("race: commit alias: %w", err)
		}
		var img models.Image
		var isMissingInt int
		if err := database.Read.QueryRow(
			`SELECT id, sha256, canonical_path, folder_path, file_type, file_size, is_missing FROM images WHERE id = ?`,
			existingID,
		).Scan(&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType, &img.FileSize, &isMissingInt); err != nil {
			return nil, true, fmt.Errorf("race: reload existing image %d: %w", existingID, err)
		}
		img.IsMissing = isMissingInt == 1
		return &img, true, nil
	}
	if insertErr != nil {
		return nil, false, fmt.Errorf("inserting image: %w", insertErr)
	}

	if _, err := tx.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical, mtime_unix, mtime_nsec) VALUES (?, ?, 1, ?, ?)`,
		imgID, path, fi.ModTime().Unix(), fi.ModTime().UnixNano(),
	); err != nil {
		return nil, false, fmt.Errorf("inserting image_path: %w", err)
	}

	if sdMeta != nil {
		sdMeta.ImageID = imgID
		if err := insertSDMeta(context.Background(), tx, sdMeta); err != nil {
			return nil, false, fmt.Errorf("inserting sd_metadata: %w", err)
		}
	}
	if comfyMeta != nil {
		comfyMeta.ImageID = imgID
		if err := insertComfyMeta(context.Background(), tx, comfyMeta); err != nil {
			return nil, false, fmt.Errorf("inserting comfyui_metadata: %w", err)
		}
	}
	if mangaMeta != nil {
		mangaMeta.ImageID = imgID
		if err := insertMangaMeta(tx, mangaMeta); err != nil {
			return nil, false, fmt.Errorf("inserting manga_metadata: %w", err)
		}
	}
	if prefilledSeries != "" {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_collections (image_id, name, position) VALUES (?, ?, NULL)`,
			imgID, prefilledSeries,
		); err != nil {
			return nil, false, fmt.Errorf("inserting image_collection: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("committing ingest: %w", err)
	}

	phash := regenerateDerived(database, thumbnailsPath, path, imgID, fileType, "ingest")
	// After the thumbnail, which is what the greyscale probe reads.
	if err := ApplyMetaTags(database, thumbnailsPath, imgID); err != nil {
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
	return img, false, nil
}

func toNullInt(v *int) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func toNullFloat(v *float64) interface{} {
	if v == nil {
		return nil
	}
	return *v
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

// ReplaceGenerationMetadata drops the old rows even when no new ones
// arrive: a rewrite can strip a recipe.
func ReplaceGenerationMetadata(ctx context.Context, tx *sql.Tx, imageID int64, sd *models.SDMetadata, comfy *models.ComfyUIMetadata) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sd_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("clear sd_metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM comfyui_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("clear comfyui_metadata: %w", err)
	}
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
	return WriteComfyTerms(ctx, tx, comfy.ImageID, comfy.RawWorkflow)
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
		toNullInt(m.Count), m.Summary, m.Notes,
		toNullInt(m.Year), toNullInt(m.Month), toNullInt(m.Day),
		m.Writer, m.Penciller, m.Inker, m.Colorist, m.Letterer, m.CoverArtist, m.Editor, m.Publisher,
		m.Imprint, m.Genre, m.Web, m.LanguageISO, m.Format, m.Manga, m.AgeRating,
		toNullFloat(m.CommunityRating), toNullInt(m.XMLPageCount), m.RawXML,
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
