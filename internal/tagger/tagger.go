//go:build tagger

// Package tagger runs ONNX models over images and turns what they emit
// into tags.
package tagger

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tags"
	ort "github.com/yalue/onnxruntime_go"
)

func IsAvailable(cfg *config.Config) bool { return len(EnabledTaggers(cfg)) > 0 }

func buildSupportsInference() bool { return true }

func UnavailableReason(cfg *config.Config) string {
	if IsAvailable(cfg) {
		return ""
	}
	taggers := DiscoverTaggers(cfg)
	if len(taggers) == 0 {
		return "no tagger subfolders found under paths.model_path"
	}
	for _, t := range taggers {
		if t.Enabled && !t.Available {
			return t.Reason
		}
	}
	return "no enabled tagger"
}

func CheckProviderAvailable(provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" || provider == "cpu" {
		return nil
	}
	if !config.IsValidExecutionProvider(provider) {
		return fmt.Errorf("unsupported execution provider %q", provider)
	}

	ort.SetSharedLibraryPath(sharedLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("ort init: %w", err)
	}
	defer func() { _ = ort.DestroyEnvironment() }()

	opts, err := ort.NewSessionOptions()
	if err != nil {
		return fmt.Errorf("ort session options: %w", err)
	}
	defer func() { _ = opts.Destroy() }()

	cleanup, err := appendExecutionProvider(opts, provider, "")
	if cleanup != nil {
		cleanup()
	}
	if err != nil {
		return fmt.Errorf("libonnxruntime does not support %s: %w", provider, err)
	}

	// A CUDA-capable library says nothing about the device: in a container
	// without the GPU passed in, session creation only fails at job time.
	if provider == "cuda" && runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/nvidia0"); err != nil {
			return fmt.Errorf("no NVIDIA GPU device found (pass the GPU into the container, e.g. Podman AddDevice=nvidia.com/gpu=all)")
		}
	}
	return nil
}

func AvailableTaggers(cfg *config.Config) []TaggerStatus { return DiscoverTaggers(cfg) }

func Status() CacheStatus {
	if b := activeBackend(); b != nil {
		return b.Status()
	}
	return CacheStatus{}
}

func ReleaseIdle(after time.Duration) bool {
	if b := activeBackend(); b != nil {
		return b.ReleaseIdle(after)
	}
	return false
}

func ReleaseAll() {
	if b := activeBackend(); b != nil {
		b.ReleaseAll()
	}
}

// A whole-library scope would otherwise extract every video's frames to
// disk before the first tag lands.
const autotagChunkSize = 200

// RunWithTaggers takes only enabled, available taggers and returns how
// many ids were left untagged. provider overrides the configured one; an
// empty mangaCacheDir extracts pages under the system temp directory.
func RunWithTaggers(ctx context.Context, database *db.DB, cfg *config.Config, ids []int64, taggers []TaggerStatus, mgr *jobs.Manager, provider string, mangaCacheDir string) (int, error) {
	if len(taggers) == 0 {
		return 0, fmt.Errorf("no tagger is enabled or available")
	}
	backend := activeBackend()
	if backend == nil {
		return 0, fmt.Errorf("auto-tagger disabled (no backend registered)")
	}

	type catRow struct {
		id   int64
		name string
	}
	catIDs := map[string]int64{}
	cats, err := db.QueryAllContext(ctx, database.Read, func(rows *sql.Rows) (catRow, error) {
		var c catRow
		err := rows.Scan(&c.id, &c.name)
		return c, err
	}, `SELECT id, name FROM tag_categories`)
	if err != nil {
		logx.Warnf("tagger: read tag_categories: %v", err)
	}
	for _, c := range cats {
		catIDs[c.name] = c.id
	}
	generalCatID := catIDs["general"]

	// single_general taggers can't tell a character from a general tag,
	// so a name found in exactly one other category (bar general and
	// meta) is filed there.
	inferredCats := map[string]int64{}
	hasSingleGeneral := false
	for _, t := range taggers {
		profile, perr := ResolveProfile(cfg.Paths.ModelPath, t.Name, t.TagsFile)
		if perr == nil && profile.CategoryScheme == "single_general" {
			hasSingleGeneral = true
			break
		}
	}
	if hasSingleGeneral && generalCatID != 0 {
		// A name the user has tagged as general by hand stays general.
		inferred, err := db.QueryAllContext(ctx, database.Read, func(rows *sql.Rows) (catRow, error) {
			var c catRow
			err := rows.Scan(&c.name, &c.id)
			return c, err
		}, `
			SELECT t.name, t.category_id
			FROM tags t
			JOIN tag_categories tc ON tc.id = t.category_id
			WHERE t.is_alias = 0
			  AND tc.name NOT IN ('general', 'meta')
			  AND NOT EXISTS (
			      SELECT 1 FROM tags g
			      JOIN image_tags it ON it.tag_id = g.id
			      WHERE g.name = t.name
			        AND g.category_id = ?
			        AND g.is_alias = 0
			        AND it.is_auto = 0
			  )`, generalCatID)
		if err == nil {
			ambiguous := map[string]bool{}
			for _, r := range inferred {
				if ambiguous[r.name] {
					continue
				}
				if existing, ok := inferredCats[r.name]; ok && existing != r.id {
					ambiguous[r.name] = true
					delete(inferredCats, r.name)
					continue
				}
				inferredCats[r.name] = r.id
			}
		}
	}

	parallel := min(max(1, cfg.Tagger.Parallel), len(ids))

	// The job has one status line, so each worker writes its own slot and
	// every update joins them all.
	total := len(ids)
	var completed atomic.Int64
	var statusMu sync.Mutex
	workerStatus := make([]string, parallel)
	// More than three workers' pages overflow the status slot.
	const maxVisibleWorkers = 3
	emitStatus := func(workerIdx int, msg string) {
		statusMu.Lock()
		defer statusMu.Unlock()
		workerStatus[workerIdx] = msg
		active := slices.DeleteFunc(slices.Clone(workerStatus), func(s string) bool { return s == "" })
		out := "tagging images"
		if len(active) > 0 {
			shown := active
			if len(shown) > maxVisibleWorkers {
				shown = shown[:maxVisibleWorkers]
			}
			out = strings.Join(shown, " · ")
			if extra := len(active) - len(shown); extra > 0 {
				out = fmt.Sprintf("%s (+%d more)", out, extra)
			}
		}
		mgr.Update(int(completed.Load()), total, out)
	}

	taggerNames := make([]string, 0, len(taggers))
	for _, t := range taggers {
		taggerNames = append(taggerNames, t.Name)
	}

	var skipped atomic.Int64
	prepared := 0
	runChunk := func(chunk []int64) error {
		requests := make([]BackendImageRequest, 0, len(chunk))
		cleanups := make([]func(), 0, len(chunk))
		defer func() {
			for _, c := range cleanups {
				c()
			}
		}()
		for _, imageID := range chunk {
			if ctx.Err() != nil {
				break
			}
			// Extraction can take minutes on a video-heavy scope, so it
			// reports progress too.
			prepared++
			mgr.Update(int(completed.Load()), total, fmt.Sprintf("preparing %d/%d", prepared, total))
			var canonPath, fileType string
			if err := database.Read.QueryRowContext(ctx,
				`SELECT canonical_path, file_type FROM images WHERE id = ?`, imageID,
			).Scan(&canonPath, &fileType); err != nil {
				logx.Warnf("tagger: skip image %d: lookup failed: %v", imageID, err)
				skipped.Add(1)
				continue
			}
			framePaths, cleanup := framesForTagging(canonPath, fileType, mangaCacheDir, imageID)
			if len(framePaths) == 0 {
				logx.Warnf("tagger: skip image %d: no frames available (missing file, archive, or ffmpeg)", imageID)
				skipped.Add(1)
				cleanup()
				continue
			}
			requests = append(requests, BackendImageRequest{
				ID:            imageID,
				FramePaths:    framePaths,
				MangaProgress: fileType == "cbz" && len(framePaths) > 1,
			})
			cleanups = append(cleanups, cleanup)
		}

		resp, err := backend.Run(ctx, RunRequest{
			Cfg:            cfg,
			Taggers:        taggers,
			Provider:       provider,
			CatIDs:         catIDs,
			GeneralCatID:   generalCatID,
			InferredCats:   inferredCats,
			MinHitFraction: cfg.Tagger.Aggregation.MinHitFraction,
			Parallel:       parallel,
			Images:         requests,
			OnProgress: func(workerIdx int, msg string) {
				if msg == "" {
					completed.Add(1)
				}
				emitStatus(workerIdx, msg)
			},
		})
		if err != nil {
			return err
		}

		for _, r := range resp.Results {
			if r.Err != "" {
				skipped.Add(1)
				continue
			}
			if r.Tags == nil {
				// Cancelled mid-image - skip writing partial state.
				continue
			}
			if storeErr := storeResults(ctx, database, r.ID, r.Tags, taggerNames, catIDs["rating"]); storeErr != nil {
				logx.Warnf("tagger: store results for image %d: %v", r.ID, storeErr)
				skipped.Add(1)
			}
		}
		return nil
	}

	for start := 0; start < total; start += autotagChunkSize {
		if ctx.Err() != nil {
			break
		}
		if err := runChunk(ids[start:min(start+autotagChunkSize, total)]); err != nil {
			return int(skipped.Load()), err
		}
	}

	mgr.Update(int(completed.Load()), total, "tagging images")
	return int(skipped.Load()), ctx.Err()
}

func framesForTagging(canonPath, fileType, mangaCacheDir string, imageID int64) ([]string, func()) {
	switch fileType {
	case "mp4", "webm":
		positions := []float64{0.10, 0.30, 0.50, 0.70, 0.90}
		frames, _ := gallery.ExtractVideoFrames(canonPath, os.TempDir(), positions)
		cleanup := func() {
			for _, p := range frames {
				_ = os.Remove(p)
			}
		}
		return frames, cleanup
	case "avif", "jxl":
		frame, err := gallery.RenderStillFrame(canonPath, os.TempDir())
		if err != nil {
			return nil, func() {}
		}
		return []string{frame}, func() { _ = os.Remove(frame) }
	case "cbz":
		archive, err := gallery.OpenManga(canonPath)
		if err != nil {
			logx.Warnf("tagger: open manga %q: %v", canonPath, err)
			return nil, func() {}
		}
		pageCount := len(archive.Pages)
		_ = archive.Close()
		// Beside the reader's cache, not in it: its reclaimer unlinks pages
		// idle past their TTL, which inference over a chunk can outlast.
		root := ""
		if mangaCacheDir != "" {
			root = filepath.Dir(mangaCacheDir)
		}
		tempDir, err := os.MkdirTemp(root, "autotag-*")
		if err != nil {
			logx.Warnf("tagger: temp dir for manga frames: %v", err)
			return nil, func() {}
		}
		paths := make([]string, 0, pageCount)
		for i := 1; i <= pageCount; i++ {
			path, err := gallery.EnsureMangaPageInCache(tempDir, canonPath, imageID, i)
			if err != nil {
				logx.Warnf("tagger: extract page %d of %q: %v", i, canonPath, err)
				continue
			}
			if gallery.IsFFmpegStill(gallery.ExtFileType(path)) {
				frame, err := gallery.RenderStillFrame(path, tempDir)
				if err != nil {
					logx.Warnf("tagger: render page %d of %q: %v", i, canonPath, err)
					continue
				}
				path = frame
			}
			paths = append(paths, path)
		}
		return paths, func() { _ = os.RemoveAll(tempDir) }
	}
	return []string{canonPath}, func() {}
}

func storeResults(
	ctx context.Context, database *db.DB,
	imageID int64, merged map[TagKey]Scored, taggerNames []string, ratingCatID int64,
) error {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Aliases resolve to their canonical: an alias never goes on an image.
	type target struct {
		score      float32
		taggerName string
	}
	targets := make(map[int64]target, len(merged))
	hasRating := false
	for k, s := range merged {
		if ratingCatID != 0 && k.CatID == ratingCatID {
			// A mapping rule can name anything; only the four levels rate.
			if !tags.IsCanonicalRating(k.Name) {
				continue
			}
			hasRating = true
		}
		var tagID int64
		var isAlias int
		var canonicalID sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT id, is_alias, canonical_tag_id FROM tags WHERE name = ? AND category_id = ?`, k.Name, k.CatID,
		).Scan(&tagID, &isAlias, &canonicalID)
		if err == sql.ErrNoRows {
			res, err2 := tx.ExecContext(ctx,
				`INSERT INTO tags (name, category_id, usage_count, origin) VALUES (?, ?, 0, ?)`, k.Name, k.CatID, s.TaggerName)
			if err2 != nil {
				return fmt.Errorf("insert tag %q (cat=%d): %w", k.Name, k.CatID, err2)
			}
			tagID, _ = res.LastInsertId()
		} else if err != nil {
			return fmt.Errorf("lookup tag %q (cat=%d): %w", k.Name, k.CatID, err)
		} else if isAlias == 1 && canonicalID.Valid {
			tagID = canonicalID.Int64
		}
		if prev, ok := targets[tagID]; !ok || s.Score > prev.score {
			targets[tagID] = target{score: s.Score, taggerName: s.TaggerName}
		}
	}

	type rowInfo struct {
		isAuto     bool
		taggerName string
	}
	current := map[int64]rowInfo{}
	type tagRow struct {
		id int64
		rowInfo
	}
	existing, err := db.QueryAllContext(ctx, tx, func(rows *sql.Rows) (tagRow, error) {
		var r tagRow
		var isAuto int
		var tname sql.NullString
		err := rows.Scan(&r.id, &isAuto, &tname)
		r.isAuto, r.taggerName = isAuto == 1, tname.String
		return r, err
	}, `SELECT tag_id, is_auto, tagger_name FROM image_tags WHERE image_id = ?`, imageID)
	if err != nil {
		return err
	}
	for _, r := range existing {
		current[r.id] = r.rowInfo
	}

	toRemove := map[int64]struct{}{}
	if len(taggerNames) > 0 {
		scope := make(map[string]struct{}, len(taggerNames))
		for _, n := range taggerNames {
			scope[n] = struct{}{}
		}
		for tid, info := range current {
			if !info.isAuto {
				continue
			}
			if _, ok := scope[info.taggerName]; !ok {
				continue
			}
			if _, keep := targets[tid]; keep {
				continue
			}
			toRemove[tid] = struct{}{}
		}
	}
	toAdd := map[int64]target{}
	for tid, t := range targets {
		if _, exists := current[tid]; !exists {
			toAdd[tid] = t
		}
	}

	for tid := range toRemove {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ? AND is_auto = 1`, imageID, tid); err != nil {
			return fmt.Errorf("remove auto tag %d: %w", tid, err)
		}
		if err := tags.DropTagUsageTx(tx, tid, imageID); err != nil {
			return fmt.Errorf("decrement usage for tag %d: %w", tid, err)
		}
	}

	for tid, t := range targets {
		info, exists := current[tid]
		if !exists || !info.isAuto {
			continue
		}
		var tname any
		if t.taggerName != "" {
			tname = t.taggerName
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE image_tags SET confidence = ?, tagger_name = ? WHERE image_id = ? AND tag_id = ? AND is_auto = 1`,
			t.score, tname, imageID, tid); err != nil {
			return fmt.Errorf("refresh attribution for tag %d: %w", tid, err)
		}
	}

	// Existing rows are recorded too: the ledger captures a tagger
	// re-confirming a tag.
	for tid, t := range targets {
		if err := tags.RecordTagSourceTx(tx, imageID, tid, t.taggerName); err != nil {
			return fmt.Errorf("record tag source %d: %w", tid, err)
		}
	}

	var inserted []int64
	for tid, t := range toAdd {
		var tname any
		if t.taggerName != "" {
			tname = t.taggerName
		}
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name) VALUES (?, ?, 1, 0, ?, ?)`,
			imageID, tid, t.score, tname)
		if err != nil {
			return fmt.Errorf("insert auto tag %d: %w", tid, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		// Through the tags helpers: a missing image is not counted in
		// usage_count.
		if err := tags.BumpTagUsageTx(tx, tid, imageID); err != nil {
			return fmt.Errorf("increment usage for tag %d: %w", tid, err)
		}
		inserted = append(inserted, tid)
	}
	// After every insert: a parent fanned out first would leave its emitted child implied.
	for _, tid := range inserted {
		if err := tags.ApplyImpliedFanoutTx(tx, imageID, tid, ratingCatID, true); err != nil {
			return fmt.Errorf("fan out implications for tag %d: %w", tid, err)
		}
	}

	// A tagger can emit several ratings in one pass; keep the highest, as
	// search resolves it.
	if hasRating {
		if err := tags.PruneLowerRatingsTx(tx, ratingCatID, imageID); err != nil {
			return fmt.Errorf("prune lower ratings: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE images SET auto_tagged_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), imageID); err != nil {
		return fmt.Errorf("stamp auto_tagged_at on image %d: %w", imageID, err)
	}

	return tx.Commit()
}
