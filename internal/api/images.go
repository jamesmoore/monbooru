package api

import (
	"bytes"
	"cmp"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/markup"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
)

func linkParentRelations(g Gallery, imageID int64, url, parentURL string) {
	if g.RelationsSvc == nil {
		return
	}
	if parentURL != "" {
		if parentID, ok := gallery.ImageIDBySourceURL(g.DB, parentURL); ok && parentID != imageID {
			linkFirstSource(g, parentID, imageID)
		}
	}
	if url == "" {
		return
	}
	children, err := gallery.ChildIDsByParentURL(g.DB, url)
	if err != nil {
		logx.Warnf("api: child lookup for %q: %v", url, err)
		return
	}
	for _, child := range children {
		if child == imageID {
			continue
		}
		linkFirstSource(g, imageID, child)
	}
}

// Only while the derivative names no source: a fetch must not stack its
// claim on one the operator declared by hand.
func linkFirstSource(g Gallery, source, derivative int64) {
	has, err := relations.HasDerivativeSource(g.DB, derivative)
	if err != nil {
		logx.Debugf("api: parent link %d -> %d skipped: %v", source, derivative, err)
		return
	}
	if has {
		return
	}
	if err := g.RelationsSvc.AddDerivativeEdge(source, derivative); err != nil {
		logx.Debugf("api: parent link %d -> %d skipped: %v", source, derivative, err)
	}
}

func (h *Handler) enrichImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var canonPath, storedMD5 string
	switch err := g.DB.Read.QueryRow(`SELECT canonical_path, md5 FROM images WHERE id = ?`, id).Scan(&canonPath, &storedMD5); {
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	case err != nil:
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	var body struct {
		Tags            []string         `json:"tags"`
		Source          string           `json:"source"`
		PostID          string           `json:"post_id"`
		URL             string           `json:"url"`
		SourceMD5       string           `json:"source_md5"`
		ParentURL       string           `json:"parent_url"`
		Verify          bool             `json:"verify"`
		PostWidth       int              `json:"post_width"`
		PostHeight      int              `json:"post_height"`
		PostSize        int64            `json:"post_size"`
		PostExt         string           `json:"post_ext"`
		Similarity      float64          `json:"similarity"`
		Commentary      string           `json:"commentary"`
		Translated      string           `json:"commentary_translated"`
		CommentaryDText string           `json:"commentary_dtext"`
		TranslatedDText string           `json:"commentary_translated_dtext"`
		Original        string           `json:"original"`
		Notes           []annotationJSON `json:"notes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := validateMaxLen("commentary", strings.TrimSpace(body.Commentary), maxImageCommentaryLen); badRequest(w, err) {
		return
	}
	if err := validateMaxLen("commentary_translated", strings.TrimSpace(body.Translated), maxImageCommentaryLen); badRequest(w, err) {
		return
	}
	if err := validateMaxLen("original", strings.TrimSpace(body.Original), maxImageOriginalLen); badRequest(w, err) {
		return
	}
	if !checkCommentaryDText(w, body.CommentaryDText, body.TranslatedDText) {
		return
	}
	sourceMD5 := strings.TrimSpace(body.SourceMD5)
	if err := validateMaxLen("source_md5", sourceMD5, maxSourceMD5Len); badRequest(w, err) {
		return
	}
	postID := strings.TrimSpace(body.PostID)
	if err := validateMaxLen("post_id", postID, maxSourcePostIDLen); badRequest(w, err) {
		return
	}
	postFile := postFileFrom(body.PostWidth, body.PostHeight, body.PostSize, body.PostExt)
	if err := validateMaxLen("post_ext", postFile.Ext, maxSourcePostExtLen); badRequest(w, err) {
		return
	}
	parentURL := strings.TrimSpace(body.ParentURL)
	if err := validateImageURL(parentURL); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "parent_url: "+err.Error())
		return
	}
	source := strings.TrimSpace(body.Source)
	if err := validateImageSource(source); badRequest(w, err) {
		return
	}
	if err := validateImageURL(strings.TrimSpace(body.URL)); badRequest(w, err) {
		return
	}
	verified := true
	md5Verdict := ""
	if body.Verify {
		if sourceMD5 == "" {
			verified = false
		} else {
			got := storedMD5
			if got == "" {
				var err error
				if got, err = gallery.ComputeAndStoreMD5(r.Context(), g.DB, id); err != nil {
					g.recordFetch(id, "error", "could not verify the file; fetch not applied")
					apiError(w, http.StatusInternalServerError, "internal_error", "cannot hash image: "+err.Error())
					return
				}
			}
			if !strings.EqualFold(got, sourceMD5) {
				md5Verdict = "differ"
				// A similarity-matched origin serves a different file by
				// design and applies unverified. The flag may sit on the
				// (site, "") row the merge below has not adopted yet.
				if body.Similarity <= 0 && !gallery.SourceSimilarityMatched(g.DB, id, source, postID) &&
					!gallery.SourceSimilarityMatched(g.DB, id, source, "") {
					// Recorded although the fetch is refused: the
					// [upgrade] gate reads it.
					if err := gallery.SetSourceMD5Match(g.DB, id, source, postID, md5Verdict); err != nil {
						logx.Warnf("api enrich: record md5 verdict: %v", err)
					}
					g.recordFetch(id, "mismatch", "the source returned a different file (hash mismatch); no tags applied")
					apiError(w, http.StatusConflict, "hash_mismatch", "the source returned a different file")
					return
				}
				verified = false
			} else {
				md5Verdict = "match"
			}
		}
	}
	sum, tagWarnings, err := h.mergeSource(g, id, source, postID, strings.TrimSpace(body.URL), sourceMD5, parentURL,
		postFile, body.Tags, "api")
	if err != nil {
		g.recordFetch(id, "error", "fetch failed while applying tags")
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	linkParentRelations(g, id, strings.TrimSpace(body.URL), parentURL)
	if err := gallery.SetSourceSimilarity(g.DB, id, source, postID, body.Similarity); err != nil {
		g.recordFetch(id, "error", "fetch failed while recording the match score")
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// After the merge, which creates a first enrich's origin row.
	if md5Verdict != "" {
		if err := gallery.SetSourceMD5Match(g.DB, id, source, postID, md5Verdict); err != nil {
			g.recordFetch(id, "error", "fetch failed while recording the hash verdict")
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}
	if step, err := gallery.ApplySourceProvenance(g.DB, id, source, postID,
		commentaryFromInput(body.Commentary, body.CommentaryDText),
		commentaryFromInput(body.Translated, body.TranslatedDText),
		strings.TrimSpace(body.Original),
		annotationsFromInput(body.Notes, strings.TrimSpace(body.URL))); err != nil {
		g.recordFetch(id, "error", "fetch failed while applying "+step)
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	g.invalidate()
	g.recordFetch(id, "ok", fetchSummary(sum, len(body.Tags)))
	// A hash lookup reports its hit by enriching.
	recordLookupHit(g, id, source)
	resp := map[string]any{"merge": sum, "verified": verified}
	if len(tagWarnings) > 0 {
		resp["tag_warnings"] = tagWarnings
	}
	WriteJSON(w, http.StatusOK, resp)
}

func fetchSummary(sum gallery.MergeSummary, tagsSent int) string {
	switch {
	case tagsSent == 0 && sum.SourceAdded:
		return "Recorded the source; no tags were fetched."
	case sum.TagsAdded > 0 && sum.TagsRetired > 0:
		return fmt.Sprintf("Fetched tags from the source (+%d; %d no longer listed there).", sum.TagsAdded, sum.TagsRetired)
	case sum.TagsAdded > 0:
		return fmt.Sprintf("Fetched tags from the source (+%d).", sum.TagsAdded)
	case sum.TagsRetired > 0:
		return fmt.Sprintf("Fetched tags from the source (%d no longer listed there).", sum.TagsRetired)
	default:
		return "Fetched tags from the source."
	}
}

func (h *Handler) fetchStatusReport(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var body struct {
		State   string `json:"state"`
		Message string `json:"message"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.State) == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "state is required")
		return
	}
	g.recordFetch(id, body.State, body.Message)
	recordLookupTerminal(g, id, body.State)
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func recordLookupHit(g Gallery, imageID int64, source string) {
	backend := lookup.BackendBooru
	if strings.EqualFold(strings.TrimSpace(source), "ptr") {
		backend = lookup.BackendPTR
	}
	if err := lookup.RecordInFlight(g.DB, imageID, backend, lookup.ResultHit, time.Now()); err != nil {
		logx.Warnf("api: lookup hit for image %d: %v", imageID, err)
	}
}

// Only hash_not_found says anything about the image; a dropped job or a
// failure code is about the plumbing and leaves the ladder where it was.
func recordLookupTerminal(g Gallery, imageID int64, state string) {
	var result string
	switch state {
	case "pending", "ok":
		return
	case "hash_not_found":
		result = lookup.ResultMiss
	default:
		result = lookup.ResultError
	}
	if err := lookup.RecordInFlight(g.DB, imageID, "", result, time.Now()); err != nil {
		logx.Warnf("api: lookup outcome for image %d: %v", imageID, err)
	}
}

func (h *Handler) replaceImageFile(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var curSHA, fileType, canonical string
	var oldW, oldH *int
	var oldSize int64
	switch err := g.DB.Read.QueryRow(
		`SELECT sha256, file_type, width, height, file_size, canonical_path FROM images WHERE id = ?`, id,
	).Scan(&curSHA, &fileType, &oldW, &oldH, &oldSize, &canonical); {
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	case err != nil:
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if gallery.IsVideoType(fileType) || fileType == models.FileTypeCBZ {
		g.recordFetch(id, "error", "this file type cannot be replaced")
		apiError(w, http.StatusConflict, "wrong_type", "only image rows can have their file replaced")
		return
	}
	if !gallery.NamedInside(g.GalleryPath, canonical) {
		g.recordFetch(id, "error", "the file is outside this gallery")
		apiError(w, http.StatusConflict, "conflict", "the image's file is outside the gallery root")
		return
	}
	if err := g.Boundary().Check(canonical); err != nil {
		g.recordFetch(id, "error", "the file sits in a folder this gallery leaves out")
		apiError(w, http.StatusConflict, "conflict", "the image's file is not this gallery's: "+err.Error())
		return
	}
	if !isMultipart(r.Header.Get("Content-Type")) {
		apiError(w, http.StatusBadRequest, "invalid_request", "multipart body required")
		return
	}
	file, fh, err := r.FormFile("file")
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "missing file field")
		return
	}
	defer func() { _ = file.Close() }()

	source := strings.TrimSpace(r.FormValue("source"))
	postID := strings.TrimSpace(r.FormValue("post_id"))
	url := strings.TrimSpace(r.FormValue("url"))
	postFile := postFileFrom(
		atoiOrZero(r.FormValue("post_width")),
		atoiOrZero(r.FormValue("post_height")),
		int64(atoiOrZero(r.FormValue("post_size"))),
		r.FormValue("post_ext"))
	claimedMD5 := strings.TrimSpace(r.FormValue("md5"))
	parentURL := strings.TrimSpace(r.FormValue("parent_url"))
	if !checkCommentaryDText(w, r.FormValue("commentary_dtext"), r.FormValue("commentary_translated_dtext")) {
		return
	}
	commentary := commentaryFromInput(r.FormValue("commentary"), r.FormValue("commentary_dtext"))
	translated := commentaryFromInput(r.FormValue("commentary_translated"), r.FormValue("commentary_translated_dtext"))
	original := strings.TrimSpace(r.FormValue("original"))
	notes := parseNotesField(r.FormValue("notes"), url)
	var tags []string
	if tagsJSON := r.FormValue("tags"); tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "tags must be a JSON array of names")
			return
		}
	}
	if err := validateCreateProvenance(source, postID, url, claimedMD5, parentURL, "", commentary, translated, original, postFile.Ext, nil); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Staged outside the watched gallery tree so the watcher never sees a
	// half-written file.
	staged, err := os.CreateTemp(g.ThumbnailsPath, "replace-*"+filepath.Ext(fh.Filename))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to stage upload")
		return
	}
	stagedPath := staged.Name()
	discardStaged := func() { _ = os.Remove(stagedPath) }
	shaH, md5H := sha256.New(), md5.New()
	if _, err := io.Copy(io.MultiWriter(staged, shaH, md5H), file); err != nil {
		_ = staged.Close()
		discardStaged()
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to save upload")
		return
	}
	_ = staged.Close()
	newSHA := hex.EncodeToString(shaH.Sum(nil))
	newMD5 := hex.EncodeToString(md5H.Sum(nil))

	applyMeta := func() (gallery.MergeSummary, []string, bool) {
		sum, tagWarnings, err := h.mergeSource(g, id, source, postID, url, claimedMD5, parentURL, postFile, tags, "api")
		if err != nil {
			g.recordFetch(id, "error", "replace failed while applying tags")
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return sum, nil, false
		}
		linkParentRelations(g, id, url, parentURL)
		if step, err := gallery.ApplySourceProvenance(g.DB, id, source, postID, commentary, translated, original, notes); err != nil {
			g.recordFetch(id, "error", "replace failed while applying "+step)
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return sum, tagWarnings, false
		}
		if source != "" {
			if err := gallery.MarkSourceExact(g.DB, id, source, postID, newMD5); err != nil {
				g.recordFetch(id, "error", "replace failed while recording the source state")
				apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
				return sum, tagWarnings, false
			}
		}
		return sum, tagWarnings, true
	}

	if strings.EqualFold(newSHA, curSHA) {
		discardStaged()
		sum, tagWarnings, ok := applyMeta()
		if !ok {
			return
		}
		g.invalidate()
		msg := "The file already matches the source."
		if source != "" {
			msg += " " + fetchSummary(sum, len(tags))
		}
		g.recordFetch(id, "ok", msg)
		resp := map[string]any{"replaced": false, "merge": sum}
		if len(tagWarnings) > 0 {
			resp["tag_warnings"] = tagWarnings
		}
		WriteJSON(w, http.StatusOK, resp)
		return
	}

	// Rerun whenever the staged digest moves, or the write fails on the
	// UNIQUE constraint instead of this clean 409.
	refuseHeldSHA := func() bool {
		var otherID int64
		if err := g.DB.Read.QueryRow(
			`SELECT id FROM images WHERE sha256 = ? AND id != ?`, newSHA, id,
		).Scan(&otherID); err != nil {
			return false
		}
		discardStaged()
		if g.RelationsSvc != nil {
			if err := g.RelationsSvc.AddDuplicate(otherID, id); err != nil {
				logx.Debugf("api replace: duplicate link %d - %d skipped: %v", id, otherID, err)
			}
		}
		g.recordFetch(id, "already_exists",
			fmt.Sprintf("You already hold the original as image %d.", otherID))
		apiError(w, http.StatusConflict, "already_exists",
			fmt.Sprintf("the original already exists as image %d", otherID))
		return true
	}
	if refuseHeldSHA() {
		return
	}

	newType, ftErr := gallery.DetectFileType(stagedPath)
	if ftErr != nil {
		discardStaged()
		g.recordFetch(id, "error", "the source served an unsupported file type")
		apiError(w, http.StatusBadRequest, "unsupported_type", "unsupported or unrecognised file type")
		return
	}
	if magic, err := gallery.MagicFileType(stagedPath); err == nil {
		newType = magic
	}
	if gallery.IsVideoType(newType) || newType == models.FileTypeCBZ {
		discardStaged()
		g.recordFetch(id, "error", "the source serves a video or archive; only image files can replace an image")
		apiError(w, http.StatusConflict, "wrong_type", "the replacement must be an image file")
		return
	}
	if !gallery.IsFFmpegStill(newType) && !canDecodeImage(stagedPath) {
		rescued := newType == models.FileTypeJPEG && jpegBytes(stagedPath) &&
			gallery.NormalizeImage(stagedPath) == nil && canDecodeImage(stagedPath)
		if !rescued {
			discardStaged()
			g.recordFetch(id, "error", "the downloaded file does not decode as an image")
			apiError(w, http.StatusUnsupportedMediaType, "unsupported_type", "file does not decode as an image")
			return
		}
		if reSHA, err := gallery.HashFile(stagedPath); err == nil && reSHA != newSHA {
			newSHA = reSHA
			if refuseHeldSHA() {
				return
			}
		}
		if reMD5, err := gallery.Md5File(stagedPath); err == nil {
			newMD5 = reMD5
		}
	}

	phash, err := gallery.ApplyReplacedFile(g.DB, g.Boundary(), g.ThumbnailsPath, id, stagedPath, newSHA, newMD5, newType)
	if err != nil {
		discardStaged()
		logx.Warnf("api replace image %d: %v", id, err)
		g.recordFetch(id, "error", "the file replacement failed")
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	relations.PhashSink(g.DB).Stored(id, phash)

	sum, tagWarnings, ok := applyMeta()
	if !ok {
		return
	}
	g.invalidate()

	var newW, newH *int
	var newSize int64
	_ = g.DB.Read.QueryRow(`SELECT width, height, file_size FROM images WHERE id = ?`, id).
		Scan(&newW, &newH, &newSize)
	msg := fmt.Sprintf("Replaced the file (%s -> %s, %d kB -> %d kB).",
		dimsLabel(oldW, oldH), dimsLabel(newW, newH), (oldSize+512)/1024, (newSize+512)/1024)
	if source != "" {
		msg += " " + fetchSummary(sum, len(tags))
	}
	g.recordFetch(id, "ok", msg)
	resp := map[string]any{"replaced": true, "merge": sum}
	if len(tagWarnings) > 0 {
		resp["tag_warnings"] = tagWarnings
	}
	WriteJSON(w, http.StatusOK, resp)
}

func dimsLabel(w, h *int) string {
	if w == nil || h == nil {
		return "?x?"
	}
	return fmt.Sprintf("%dx%d", *w, *h)
}

func canDecodeImage(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	_, _, err = image.DecodeConfig(f)
	return err == nil
}

// ffmpeg decodes whatever it can read, so a file merely named .jpg must
// not reach the rescue.
func jpegBytes(path string) bool {
	t, err := gallery.MagicFileType(path)
	return err == nil && t == models.FileTypeJPEG
}

func undecodableJPEG(f io.ReadSeeker) bool {
	defer func() { _, _ = f.Seek(0, io.SeekStart) }()
	magic := make([]byte, 3)
	if _, err := io.ReadFull(f, magic); err != nil || !bytes.Equal(magic, []byte{0xFF, 0xD8, 0xFF}) {
		return false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}
	_, _, err := image.DecodeConfig(f)
	return err != nil
}

// The rescue runs in dir, outside the watched gallery, so the watcher
// never sees the bytes it replaces. The caller removes the file.
func rescueJPEG(dir string, src io.Reader) (*os.File, error) {
	staged, err := os.CreateTemp(dir, "rescue-*.jpg")
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(staged, src)
	_ = staged.Close()
	if err == nil {
		err = gallery.NormalizeImage(staged.Name())
	}
	if err == nil && !canDecodeImage(staged.Name()) {
		err = errors.New("the rescued file does not decode")
	}
	if err == nil {
		var rescued *os.File
		if rescued, err = os.Open(staged.Name()); err == nil {
			return rescued, nil
		}
	}
	_ = os.Remove(staged.Name())
	return nil, err
}

func (h *Handler) buildImageResponse(g Gallery, imageID int64) (*imageResponse, error) {
	img, err := models.ScanImageRow(g.DB.Read.QueryRow(
		`SELECT `+models.ImageRowColumns+` FROM images i WHERE i.id = ?`, imageID))
	if err != nil {
		return nil, err
	}

	var aliases []string
	if byID, err := loadAliasesForImages(g, []int64{imageID}); err != nil {
		logx.Warnf("buildImageResponse aliases: %v", err)
	} else {
		aliases = byID[imageID]
	}
	var tags []imageTagJSON
	if byID, err := loadTagsForImages(g, []int64{imageID}); err != nil {
		logx.Warnf("buildImageResponse tags: %v", err)
	} else {
		tags = byID[imageID]
	}

	resp := makeImageResponse(g, img, tags, aliases)
	if srcs, err := loadTagSourcesForImage(g, imageID); err != nil {
		logx.Warnf("buildImageResponse tag sources: %v", err)
	} else {
		resp.TagSources = srcs
	}
	if cols, err := gallery.CollectionsForImage(g.DB, imageID); err != nil {
		logx.Warnf("buildImageResponse collections: %v", err)
	} else {
		for _, c := range cols {
			resp.Collections = append(resp.Collections, collectionJSON{Name: c.Name, Order: c.Order})
		}
	}
	if srcs, err := gallery.SourcesForImage(g.DB, imageID); err != nil {
		logx.Warnf("buildImageResponse sources: %v", err)
	} else {
		for _, s := range srcs {
			resp.Sources = append(resp.Sources, sourceJSON{Site: s.Site, PostID: s.PostID, URL: s.URL, Commentary: s.Commentary,
				CommentaryTranslated: s.CommentaryTranslated, Original: s.Original, Similarity: s.Similarity})
		}
	}
	if anns, err := gallery.AnnotationsForImage(g.DB, imageID); err != nil {
		logx.Warnf("buildImageResponse annotations: %v", err)
	} else {
		for _, a := range anns {
			aj := annotationJSON{Site: a.Site, PostID: a.PostID, X: a.X, Y: a.Y, W: a.W, H: a.H, Body: a.Body}
			if text := markup.Parse(a.Body).Text(); text != a.Body {
				aj.BodyText = text
			}
			resp.Annotations = append(resp.Annotations, aj)
		}
	}
	addLookupState(g, imageID, &resp)
	return &resp, nil
}

func addLookupState(g Gallery, imageID int64, resp *imageResponse) {
	var on, ptrOn bool
	if err := g.DB.Read.QueryRow(
		`SELECT scheduled_lookup, scheduled_lookup_ptr FROM images WHERE id = ?`, imageID,
	).Scan(&on, &ptrOn); err != nil {
		logx.Warnf("buildImageResponse scheduled_lookup: %v", err)
		return
	}
	resp.ScheduledLookup, resp.ScheduledLookupPTR = &on, &ptrOn
	rows, err := lookup.ForImage(g.DB, imageID)
	if err != nil {
		logx.Warnf("buildImageResponse lookup history: %v", err)
		return
	}
	for backend, r := range rows {
		entry := lookupJSON{LastResult: r.LastResult, Attempts: r.Attempts}
		if !r.LastAt.IsZero() {
			entry.LastAt = r.LastAt.Format(time.RFC3339)
		}
		if !r.NextDueAt.IsZero() {
			entry.NextDueAt = r.NextDueAt.Format(time.RFC3339)
		}
		if resp.Lookup == nil {
			resp.Lookup = map[string]lookupJSON{}
		}
		resp.Lookup[backend] = entry
	}
}

func (h *Handler) getImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}

	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) patchImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndExistingID(w, r)
	if !ok {
		return
	}

	var body struct {
		Source             *string `json:"source"`
		URL                *string `json:"url"`
		Collection         *string `json:"collection"`
		CollectionOrder    *int    `json:"collection_order"`
		IsFavorited        *bool   `json:"is_favorited"`
		IsInbox            *bool   `json:"is_inbox"`
		ScheduledLookup    *bool   `json:"scheduled_lookup"`
		ScheduledLookupPTR *bool   `json:"scheduled_lookup_ptr"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	updates := []string{}
	args := []any{}
	cacheAffecting := false

	setSrc := false
	var srcSite, srcURL string
	if body.Source != nil || body.URL != nil {
		if err := g.DB.Read.QueryRow(`SELECT source, url FROM images WHERE id = ?`, id).Scan(&srcSite, &srcURL); serverError(w, err) {
			return
		}
		if body.Source != nil {
			s := strings.TrimSpace(*body.Source)
			if err := validateImageSource(s); badRequest(w, err) {
				return
			}
			srcSite = s
		}
		if body.URL != nil {
			u := strings.TrimSpace(*body.URL)
			if err := validateImageURL(u); badRequest(w, err) {
				return
			}
			srcURL = u
		}
		setSrc = true
		cacheAffecting = true
	}
	setHome := false
	var homeName string
	var homeOrder *int
	if body.Collection != nil || body.CollectionOrder != nil {
		var curSeries string
		var curOrder sql.NullInt64
		if err := g.DB.Read.QueryRow(`SELECT series, series_order FROM images WHERE id = ?`, id).Scan(&curSeries, &curOrder); serverError(w, err) {
			return
		}
		homeName = curSeries
		if body.Collection != nil {
			c := strings.TrimSpace(*body.Collection)
			if err := validateImageCollection(c); badRequest(w, err) {
				return
			}
			homeName = c
		}
		if body.CollectionOrder != nil {
			n := *body.CollectionOrder
			if n < 1 {
				apiError(w, http.StatusBadRequest, "invalid_request", "collection_order must be 1 or higher")
				return
			}
			if homeName == "" {
				apiError(w, http.StatusBadRequest, "invalid_request", "collection_order requires a non-empty collection")
				return
			}
			homeOrder = &n
		} else if homeName != "" && curOrder.Valid {
			v := int(curOrder.Int64)
			homeOrder = &v
		}
		setHome = true
		cacheAffecting = true
	}
	if body.IsFavorited != nil {
		updates = append(updates, "is_favorited = ?")
		args = append(args, boolToInt(*body.IsFavorited))
		cacheAffecting = true
	}
	if body.IsInbox != nil {
		updates = append(updates, "is_inbox = ?")
		args = append(args, boolToInt(*body.IsInbox))
		cacheAffecting = true
	}
	if body.ScheduledLookup != nil {
		updates = append(updates, "scheduled_lookup = ?")
		args = append(args, boolToInt(*body.ScheduledLookup))
	}
	if body.ScheduledLookupPTR != nil {
		updates = append(updates, "scheduled_lookup_ptr = ?")
		args = append(args, boolToInt(*body.ScheduledLookupPTR))
	}
	if len(updates) == 0 && !setHome && !setSrc {
		apiError(w, http.StatusBadRequest, "invalid_request", "no editable fields supplied")
		return
	}

	if len(updates) > 0 {
		args = append(args, id)
		if _, err := g.DB.Write.Exec(`UPDATE images SET `+strings.Join(updates, ", ")+` WHERE id = ?`, args...); serverError(w, err) {
			return
		}
	}
	if setHome {
		if err := gallery.SetHomeCollection(g.DB, id, homeName, homeOrder); serverError(w, err) {
			return
		}
	}
	if setSrc {
		if err := gallery.SetPrimarySource(g.DB, id, srcSite, srcURL); err != nil {
			if errors.Is(err, gallery.ErrSourceIdentityExists) {
				apiError(w, http.StatusConflict, "conflict", err.Error())
				return
			}
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}
	// Turning the schedule on resets the ladder, or an exhausted image
	// would never be tried again.
	for backend, want := range map[string]*bool{
		lookup.BackendBooru: body.ScheduledLookup,
		lookup.BackendPTR:   body.ScheduledLookupPTR,
	} {
		if want == nil || !*want {
			continue
		}
		if err := lookup.Reset(g.DB, id, backend, time.Now()); err != nil {
			logx.Warnf("api: lookup reset for image %d: %v", id, err)
		}
	}
	if cacheAffecting {
		g.invalidate()
	}

	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	WriteJSON(w, http.StatusOK, resp)
}

func atoiOrZero(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func checkCommentaryDText(w http.ResponseWriter, dtext, translatedDText string) bool {
	if err := validateMaxLen("commentary_dtext", dtext, maxImageCommentaryDTextLen); badRequest(w, err) {
		return false
	}
	if err := validateMaxLen("commentary_translated_dtext", translatedDText, maxImageCommentaryDTextLen); badRequest(w, err) {
		return false
	}
	return true
}

func checkCreateProvenance(w http.ResponseWriter, in createInput) bool {
	if err := validateCreateProvenance(in.source, in.postID, in.url, in.md5, in.parentURL,
		in.collection, in.commentary, in.translated, in.original, in.postFile.Ext, in.collectionOrder); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	return true
}

func postFileFrom(w, h int, size int64, ext string) gallery.PostFile {
	return gallery.PostFile{
		Width:  max(w, 0),
		Height: max(h, 0),
		Size:   max(size, 0),
		Ext:    strings.TrimSpace(ext),
	}
}

type createInput struct {
	imgPath         string
	initialTags     []string
	folder          string
	autotag         bool
	taggerName      string
	via             string
	source          string
	postID          string
	url             string
	md5             string // md5 the source claimed
	postFile        gallery.PostFile
	parentURL       string
	commentary      string
	translated      string
	original        string
	notes           []models.Annotation
	collection      string
	collectionOrder *int
	uploadedToDisk  bool
	naming          gallery.Naming
}

func (h *Handler) parseCreateMultipart(w http.ResponseWriter, r *http.Request, g Gallery) (createInput, bool) {
	var in createInput
	file, fh, err := r.FormFile("file")
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "missing file field")
		return in, false
	}
	defer func() { _ = file.Close() }()

	in.folder = strings.TrimSpace(r.FormValue("folder"))
	in.autotag = isTrue(r.FormValue("autotag"))
	in.taggerName = strings.TrimSpace(r.FormValue("tagger_name"))
	in.via = strings.TrimSpace(r.FormValue("via"))
	if err := validateVia(in.via); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return in, false
	}
	in.source = strings.TrimSpace(r.FormValue("source"))
	in.postID = strings.TrimSpace(r.FormValue("post_id"))
	in.url = strings.TrimSpace(r.FormValue("url"))
	in.md5 = strings.TrimSpace(r.FormValue("md5"))
	in.postFile = postFileFrom(
		atoiOrZero(r.FormValue("post_width")),
		atoiOrZero(r.FormValue("post_height")),
		int64(atoiOrZero(r.FormValue("post_size"))),
		r.FormValue("post_ext"))
	in.parentURL = strings.TrimSpace(r.FormValue("parent_url"))
	if !checkCommentaryDText(w, r.FormValue("commentary_dtext"), r.FormValue("commentary_translated_dtext")) {
		return in, false
	}
	in.commentary = commentaryFromInput(r.FormValue("commentary"), r.FormValue("commentary_dtext"))
	in.translated = commentaryFromInput(r.FormValue("commentary_translated"), r.FormValue("commentary_translated_dtext"))
	in.original = strings.TrimSpace(r.FormValue("original"))
	in.notes = parseNotesField(r.FormValue("notes"), in.url)
	if tagsJSON := r.FormValue("tags"); tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &in.initialTags); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "tags must be a JSON array of names")
			return in, false
		}
	}
	in.collection = strings.TrimSpace(r.FormValue("collection"))
	if raw := strings.TrimSpace(r.FormValue("collection_order")); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "collection_order must be an integer")
			return in, false
		}
		in.collectionOrder = &n
	}
	if !checkCreateProvenance(w, in) {
		return in, false
	}

	defaultFolder, defaultName := h.uploadDestination()
	writeDir, naming := gallery.ReceivedNaming(g.Name, in.folder, defaultFolder, defaultName)
	in.naming = naming

	destDir, destErr := g.Boundary().ResolveSubdir(writeDir)
	if destErr != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", destErr.Error())
		return in, false
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to create folder: "+err.Error())
		return in, false
	}

	var body io.Reader = file
	if undecodableJPEG(file) {
		rescued, err := rescueJPEG(g.ThumbnailsPath, file)
		if err != nil {
			apiError(w, http.StatusUnsupportedMediaType, "unsupported_type", "file does not decode as an image")
			return in, false
		}
		defer func() {
			_ = rescued.Close()
			_ = os.Remove(rescued.Name())
		}()
		body = rescued
	}

	// Straight to the final name: the watcher would record a temp name
	// and mark it missing after the rename.
	dstPath := gallery.UniqueDestPath(destDir, fh.Filename)
	if err := g.Boundary().Check(dstPath); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return in, false
	}
	dst, err := os.Create(dstPath)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to create destination file")
		return in, false
	}
	if _, err := io.Copy(dst, body); err != nil {
		_ = dst.Close()
		_ = os.Remove(dstPath)
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to save upload")
		return in, false
	}
	_ = dst.Close()

	in.imgPath = dstPath
	in.uploadedToDisk = true
	return in, true
}

func (h *Handler) parseCreateJSON(w http.ResponseWriter, r *http.Request, g Gallery) (createInput, bool) {
	var in createInput
	var body struct {
		Path            string           `json:"path"`
		Tags            []string         `json:"tags"`
		Folder          string           `json:"folder"`
		Autotag         bool             `json:"autotag"`
		TaggerName      string           `json:"tagger_name"`
		Via             string           `json:"via"`
		Source          string           `json:"source"`
		PostID          string           `json:"post_id"`
		URL             string           `json:"url"`
		MD5             string           `json:"md5"`
		ParentURL       string           `json:"parent_url"`
		Commentary      string           `json:"commentary"`
		Translated      string           `json:"commentary_translated"`
		CommentaryDText string           `json:"commentary_dtext"`
		TranslatedDText string           `json:"commentary_translated_dtext"`
		Original        string           `json:"original"`
		Notes           []annotationJSON `json:"notes"`
		Collection      string           `json:"collection"`
		CollectionOrder *int             `json:"collection_order"`
		PostWidth       int              `json:"post_width"`
		PostHeight      int              `json:"post_height"`
		PostSize        int64            `json:"post_size"`
		PostExt         string           `json:"post_ext"`
	}
	if !decodeJSON(w, r, &body) {
		return in, false
	}
	if body.Path == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "path is required")
		return in, false
	}
	in.imgPath = body.Path
	in.initialTags = body.Tags
	in.folder = strings.TrimSpace(body.Folder)
	in.autotag = body.Autotag
	in.taggerName = strings.TrimSpace(body.TaggerName)
	in.via = strings.TrimSpace(body.Via)
	if err := validateVia(in.via); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return in, false
	}
	in.source = strings.TrimSpace(body.Source)
	in.postID = strings.TrimSpace(body.PostID)
	in.url = strings.TrimSpace(body.URL)
	in.md5 = strings.TrimSpace(body.MD5)
	in.postFile = postFileFrom(body.PostWidth, body.PostHeight, body.PostSize, body.PostExt)
	in.parentURL = strings.TrimSpace(body.ParentURL)
	if !checkCommentaryDText(w, body.CommentaryDText, body.TranslatedDText) {
		return in, false
	}
	in.commentary = commentaryFromInput(body.Commentary, body.CommentaryDText)
	in.translated = commentaryFromInput(body.Translated, body.TranslatedDText)
	in.original = strings.TrimSpace(body.Original)
	in.notes = annotationsFromInput(body.Notes, in.url)
	in.collection = strings.TrimSpace(body.Collection)
	in.collectionOrder = body.CollectionOrder
	if !checkCreateProvenance(w, in) {
		return in, false
	}

	if in.folder != "" && !filepath.IsAbs(in.imgPath) {
		destDir, destErr := g.Boundary().ResolveSubdir(in.folder)
		if destErr != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", destErr.Error())
			return in, false
		}
		in.imgPath = filepath.Join(destDir, in.imgPath)
	}

	// A row outside the gallery would let a later DELETE unlink files the
	// operator never meant to manage.
	absPath, absErr := filepath.Abs(in.imgPath)
	if absErr != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid path")
		return in, false
	}
	galleryAbs, gErr := filepath.Abs(g.GalleryPath)
	if gErr != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "gallery path unresolvable")
		return in, false
	}
	if !gallery.PathInside(galleryAbs, absPath) {
		apiError(w, http.StatusBadRequest, "invalid_request", "path must be inside the gallery root")
		return in, false
	}
	if err := g.Boundary().Check(absPath); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "path is not this gallery's: "+err.Error())
		return in, false
	}
	in.imgPath = absPath

	// A fixed message, so the response never echoes the filesystem layout.
	if _, statErr := os.Stat(in.imgPath); os.IsNotExist(statErr) {
		apiError(w, http.StatusBadRequest, "not_found", "file not found")
		return in, false
	}
	return in, true
}

func (h *Handler) createImage(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var in createInput
	if isMultipart(r.Header.Get("Content-Type")) {
		in, ok = h.parseCreateMultipart(w, r, g)
	} else {
		in, ok = h.parseCreateJSON(w, r, g)
	}
	if !ok {
		return
	}

	// MaxBytesReader covers only the multipart body; this also bounds a
	// path reference.
	if maxMB := h.cfg().Gallery.MaxFileSizeMB; maxMB > 0 {
		if info, err := os.Stat(in.imgPath); err == nil {
			if info.Size() > int64(maxMB)*1024*1024 {
				if in.uploadedToDisk {
					_ = os.Remove(in.imgPath)
				}
				apiError(w, http.StatusRequestEntityTooLarge, "file_too_large",
					fmt.Sprintf("file exceeds max size (%d MB)", maxMB))
				return
			}
		}
	}

	fileType, ftErr := gallery.DetectFileType(in.imgPath)
	if ftErr != nil {
		if in.uploadedToDisk {
			_ = os.Remove(in.imgPath)
		}
		apiError(w, http.StatusBadRequest, "unsupported_type", "unsupported or unrecognised file type")
		return
	}
	// The bytes decide what is checked, as they decide what ingest records.
	if magic, err := gallery.MagicFileType(in.imgPath); err == nil {
		fileType = magic
	}
	if !gallery.IsVideoType(fileType) && fileType != models.FileTypeCBZ && !gallery.IsFFmpegStill(fileType) {
		if !canDecodeImage(in.imgPath) {
			if in.uploadedToDisk {
				_ = os.Remove(in.imgPath)
			}
			apiError(w, http.StatusUnsupportedMediaType, "unsupported_type", "file does not decode as an image")
			return
		}
	}

	origin := in.via
	if origin == "" {
		if in.uploadedToDisk {
			origin = models.OriginUpload
		} else {
			origin = models.OriginIngest
		}
	}

	img, isDuplicate, err := gallery.Ingest(g.DB, g.GalleryPath, g.ThumbnailsPath, in.imgPath, origin)
	if err == nil && img != nil {
		relations.PhashSink(g.DB).Stored(img.ID, img.Phash)
	}
	if err != nil {
		if in.uploadedToDisk {
			_ = os.Remove(in.imgPath)
		}
		logx.Warnf("api createImage ingest: %v", err)
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	g.invalidate()
	if isDuplicate {
		// Our own upload of bytes already held is dropped; a path
		// reference is the operator's own second copy and stays an alias.
		aliasAdded := true
		if in.uploadedToDisk {
			aliasAdded = false
			gallery.DropDuplicateCopy(g.DB, img.ID, in.imgPath, "api createImage")
		}
		sum, tagWarnings, mergeErr := h.mergeSource(g, img.ID, in.source, in.postID, in.url, in.md5, in.parentURL, in.postFile, in.initialTags, in.via)
		if mergeErr != nil {
			logx.Warnf("api createImage merge: %v", mergeErr)
			apiError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+mergeErr.Error())
			return
		}
		linkParentRelations(g, img.ID, in.url, in.parentURL)
		if step, err := gallery.ApplySourceProvenance(g.DB, img.ID, in.source, in.postID, in.commentary, in.translated, in.original, in.notes); err != nil {
			logx.Warnf("api createImage %s: %v", step, err)
			apiError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+err.Error())
			return
		}
		if in.collection != "" {
			// Additive, so a held page joins the pool without displacing
			// its home.
			if err := gallery.AddCollectionMembership(g.DB, img.ID, in.collection, in.collectionOrder); err != nil {
				logx.Warnf("api createImage collection: %v", err)
				apiError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+err.Error())
				return
			}
		}
		g.invalidate()
		resp, respErr := h.buildImageResponse(g, img.ID)
		if respErr != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", "failed to build response")
			return
		}
		envelope := map[string]any{
			"image":       resp,
			"alias_added": aliasAdded,
			"merge":       sum,
		}
		if len(tagWarnings) > 0 {
			envelope["tag_warnings"] = tagWarnings
		}
		WriteJSON(w, http.StatusOK, envelope)
		return
	}

	if _, err := in.naming.Apply(r.Context(), g.DB, g.Boundary(), img.ID, in.source, in.postID); err != nil {
		logx.Warnf("api createImage name %d: %v", img.ID, err)
	}

	if err := gallery.ApplyCreateProvenance(g.DB, img.ID, in.source, in.postID, in.url, in.md5, in.parentURL, in.collection, in.commentary, in.translated, in.original, in.postFile, in.collectionOrder); err != nil {
		logx.Warnf("api createImage provenance: %v", err)
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to set provenance fields")
		return
	}
	linkParentRelations(g, img.ID, in.url, in.parentURL)
	if in.source != "" && len(in.notes) > 0 {
		if err := gallery.ReplaceSourceAnnotations(g.DB, img.ID, in.source, in.postID, in.notes); err != nil {
			logx.Warnf("api createImage annotations: %v", err)
		}
	}

	// Attributed to the source so each source owns a prunable slice of tags.
	tagVia := in.via
	if in.source != "" {
		tagVia = in.source
	}
	_, tagWarnings := h.applyInitialTags(g, img.ID, in.initialTags, tagVia)

	var autotagNote string
	if in.autotag {
		cfg := h.cfg()
		if !tagger.IsAvailable(cfg) {
			autotagNote = "autotag skipped: tagger not available"
		} else {
			selected, selErr := tagger.SelectForGallery(h.cfg(), g.Name, in.taggerName)
			if selErr != nil {
				autotagNote = "autotag skipped: " + selErr.Error()
			} else if err := h.jobs.Start("autotag"); err != nil {
				autotagNote = "autotag skipped: a job is already running"
			} else {
				imgID := img.ID
				database := g.DB
				invalidate := g.InvalidateCaches
				mangaCache := gallery.MangaCacheDir(g.ThumbnailsPath)
				go func() {
					ctx := h.jobs.Context()
					skipped, err := tagger.RunWithTaggers(ctx, database, cfg, []int64{imgID}, selected, h.jobs, cfg.Tagger.ExecutionProvider, mangaCache)
					if invalidate != nil {
						invalidate()
					}
					if ctx.Err() != nil {
						h.jobs.Complete("auto-tagging cancelled")
						return
					}
					if err != nil {
						h.jobs.Fail(err.Error())
						return
					}
					if skipped > 0 {
						h.jobs.Complete(fmt.Sprintf("auto-tagger skipped image #%d", imgID))
						return
					}
					h.jobs.Complete(fmt.Sprintf("auto-tagged image #%d", imgID))
				}()
				autotagNote = "autotag job started"
			}
		}
	}

	resp, err := h.buildImageResponse(g, img.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to build response")
		return
	}

	if len(tagWarnings) > 0 || autotagNote != "" {
		envelope := map[string]any{"image": resp}
		if len(tagWarnings) > 0 {
			envelope["tag_warnings"] = tagWarnings
		}
		if autotagNote != "" {
			envelope["autotag"] = autotagNote
		}
		WriteJSON(w, http.StatusCreated, envelope)
		return
	}
	WriteJSON(w, http.StatusCreated, resp)
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (h *Handler) deleteImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}

	result, err := gallery.DeleteImage(g.DB, g.Boundary(), g.ThumbnailsPath, id, tags.RemoveAllTagsFromImageTx, relationsOnDelete(g.RelationsSvc))
	if err != nil {
		// Only ErrNoRows means no such id; a caller told so after a busy
		// pool or a filesystem refusal would never retry.
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, http.StatusNotFound, "not_found", "image not found")
			return
		}
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	g.invalidate()

	folderRemoved := false
	if r.URL.Query().Get("delete_empty_folder") == "true" && !result.IsMissing && result.FolderPath != "" {
		fullFolderPath := filepath.Join(g.GalleryPath, result.FolderPath)
		if !gallery.PathInside(g.GalleryPath, fullFolderPath) {
			logx.Warnf("api deleteImage: refusing to remove folder %q outside gallery root %q", fullFolderPath, g.GalleryPath)
		} else if err := g.Boundary().Check(fullFolderPath); err != nil {
			logx.Warnf("api deleteImage: leaving folder %q: %v", fullFolderPath, err)
		} else if entries, readErr := os.ReadDir(fullFolderPath); readErr == nil && len(entries) == 0 {
			if removeErr := os.Remove(fullFolderPath); removeErr == nil {
				folderRemoved = true
			} else {
				logx.Warnf("api deleteImage: failed to remove empty folder %q: %v", fullFolderPath, removeErr)
			}
		}
	}

	if folderRemoved {
		WriteJSON(w, http.StatusOK, map[string]any{
			"folder_deleted": true,
			"folder":         result.FolderPath,
		})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) searchImages(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	queryStr := q.Get("q")
	sortStr := q.Get("sort")
	sortStr = cmp.Or(sortStr, "newest")
	orderStr := q.Get("order")
	orderStr = cmp.Or(orderStr, search.DefaultOrder(sortStr))

	offset, limit := parsePage(r, h.cfg().UI.PageSize, 200)
	pageNum := offset/limit + 1

	expr := search.Parse(queryStr)
	var randomSeed int64
	if seedStr := q.Get("seed"); seedStr != "" {
		if s, err := strconv.ParseInt(seedStr, 10, 64); err == nil && s != 0 {
			randomSeed = s
		}
	}
	sq := search.Query{
		Expr:       expr,
		Sort:       sortStr,
		Order:      orderStr,
		Page:       pageNum,
		Limit:      limit,
		RandomSeed: randomSeed,
	}
	if sortStr == "order" {
		sq.OrderCollection = search.PinnedCollectionName(expr)
	}

	result, err := search.Execute(g.DB, sq)
	if serverError(w, err) {
		return
	}

	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
	}
	tagsByID, tagsErr := loadTagsForImages(g, ids)
	if tagsErr != nil {
		logx.Warnf("api searchImages tag load: %v", tagsErr)
		tagsByID = nil
	}
	aliasesByID, aliasErr := loadAliasesForImages(g, ids)
	if aliasErr != nil {
		logx.Warnf("api searchImages alias load: %v", aliasErr)
		aliasesByID = nil
	}

	images := make([]imageResponse, 0, len(result.Results))
	for _, img := range result.Results {
		images = append(images, makeImageResponse(g, img, tagsByID[img.ID], aliasesByID[img.ID]))
	}

	writePage(w, result.Page, result.Limit, result.Total, images)
}

func loadAliasesForImages(g Gallery, ids []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders, args := db.InPlaceholders(ids)
	rows, err := g.DB.Read.Query(
		`SELECT image_id, path FROM image_paths
		 WHERE is_canonical = 0 AND image_id IN (`+placeholders+`)
		 ORDER BY image_id, id`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var imageID int64
		var path string
		if err := rows.Scan(&imageID, &path); err != nil {
			return nil, err
		}
		out[imageID] = append(out[imageID], path)
	}
	return out, rows.Err()
}

func loadTagsForImages(g Gallery, ids []int64) (map[int64][]imageTagJSON, error) {
	out := make(map[int64][]imageTagJSON, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders, args := db.InPlaceholders(ids)
	rows, err := g.DB.Read.Query(`
		SELECT it.image_id, t.name, tc.name, it.is_auto, it.confidence, it.tagger_name
		FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		JOIN tag_categories tc ON tc.id = t.category_id
		WHERE it.image_id IN (`+placeholders+`)
		ORDER BY it.image_id, tc.name, t.name`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var imageID int64
		var tj imageTagJSON
		var tn *string
		if err := rows.Scan(&imageID, &tj.Name, &tj.Category, &tj.IsAuto, &tj.Confidence, &tn); err != nil {
			return nil, err
		}
		tj.TaggerName = tn
		out[imageID] = append(out[imageID], tj)
	}
	return out, rows.Err()
}

func loadTagSourcesForImage(g Gallery, imageID int64) (map[string][]string, error) {
	rows, err := g.DB.Read.Query(`
		SELECT t.name, tc.name, its.source
		FROM image_tag_sources its
		JOIN tags t ON t.id = its.tag_id
		JOIN tag_categories tc ON tc.id = t.category_id
		WHERE its.image_id = ?
		ORDER BY t.name, its.created_at, its.source`, imageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var name, cat, source string
		if err := rows.Scan(&name, &cat, &source); err != nil {
			return nil, err
		}
		key := name
		if cat != "general" {
			key = cat + ":" + name
		}
		out[key] = append(out[key], source)
	}
	return out, rows.Err()
}

func (h *Handler) listImageTags(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndExistingID(w, r)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, loadImageTagsJSON(g, id))
}

func (h *Handler) addImageTags(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndExistingID(w, r)
	if !ok {
		return
	}

	var body struct {
		Tags []string `json:"tags"`
		Via  string   `json:"via"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Tags) == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request",
			"`tags` is required and must contain at least one name")
		return
	}
	via := strings.TrimSpace(body.Via)
	if err := validateVia(via); badRequest(w, err) {
		return
	}

	_, tagWarnings := h.applyInitialTags(g, id, body.Tags, via)
	g.invalidate()

	h.writeImageTagsResponse(w, g, id, tagWarnings)
}

func (h *Handler) writeImageTagsResponse(w http.ResponseWriter, g Gallery, id int64, tagWarnings []string) {
	tags := loadImageTagsJSON(g, id)
	if len(tagWarnings) > 0 {
		WriteJSON(w, http.StatusOK, map[string]any{
			"tags":         tags,
			"tag_warnings": tagWarnings,
		})
		return
	}
	WriteJSON(w, http.StatusOK, tags)
}

// Not the full image response: its provenance, collection and lookup
// reads are freight here.
func loadImageTagsJSON(g Gallery, imageID int64) []imageTagJSON {
	byID, err := loadTagsForImages(g, []int64{imageID})
	if err != nil {
		logx.Warnf("image tags: %v", err)
		return []imageTagJSON{}
	}
	if tags := byID[imageID]; tags != nil {
		return tags
	}
	return []imageTagJSON{}
}

func imageExists(g Gallery, id int64) bool {
	var n int
	return g.DB.Read.QueryRow(`SELECT 1 FROM images WHERE id = ?`, id).Scan(&n) == nil
}

func (h *Handler) applyInitialTags(g Gallery, imgID int64, rawTags []string, via string) (int, []string) {
	// Default "api" so API tags don't read as anonymous UI adds.
	via = cmp.Or(via, "api")
	tagIDs, warnings := gallery.ResolveTagNames(g.DB, g.TagSvc, rawTags, via)
	added := 0
	if len(tagIDs) > 0 {
		results, err := g.TagSvc.AddTagsToOneImage(imgID, tagIDs, via)
		if err != nil {
			warnings = append(warnings, "apply tags: "+err.Error())
		}
		for _, r := range results {
			if r.Added {
				added++
			}
		}
	}
	return added, warnings
}

// A source owns the tags it pushes; tags pushed without one go on add-only.
func (h *Handler) mergeSource(g Gallery, id int64, source, postID, url, md5, parentURL string, post gallery.PostFile, rawTags []string, via string) (gallery.MergeSummary, []string, error) {
	sum, warnings, err := gallery.MergeSource(g.DB, g.TagSvc, id, source, postID, url, md5, parentURL, post, rawTags)
	if err != nil || source != "" || len(rawTags) == 0 {
		return sum, warnings, err
	}
	added, warns := h.applyInitialTags(g, id, rawTags, via)
	sum.TagsAdded += added
	return sum, append(warnings, warns...), nil
}

func (h *Handler) removeImageTags(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndExistingID(w, r)
	if !ok {
		return
	}

	var body struct {
		Tags []string `json:"tags"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Tags) == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request",
			"`tags` is required and must contain at least one name")
		return
	}

	var tagWarnings []string
	tagIDs := make([]int64, 0, len(body.Tags))
	for _, tagName := range body.Tags {
		tagID, err := h.resolveImageTagID(g, id, tagName)
		if err != nil {
			apiError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		if tagID == 0 {
			continue
		}
		tagIDs = append(tagIDs, tagID)
	}
	if len(tagIDs) > 0 {
		if err := g.TagSvc.RemoveTagsFromOneImage(id, tagIDs); err != nil {
			if errors.Is(err, tags.ErrTagImplied) {
				apiError(w, http.StatusConflict, "tag_implied", err.Error())
				return
			}
			tagWarnings = append(tagWarnings, "remove tags: "+err.Error())
		}
	}
	g.invalidate()

	h.writeImageTagsResponse(w, g, id, tagWarnings)
}

// A category-qualified miss falls through to the literal name, so a
// general tag named like "artist:foo" stays removable.
func (h *Handler) resolveImageTagID(g Gallery, imageID int64, tagName string) (int64, error) {
	tagName = strings.TrimSpace(tagName)
	if idx := strings.Index(tagName, ":"); idx > 0 {
		catName := tagName[:idx]
		_, ok, err := tags.CategoryIDByName(g.DB, catName)
		if err != nil {
			return 0, err
		}
		if ok {
			bareName := tagName[idx+1:]
			var tagID int64
			if err := g.DB.Read.QueryRow(
				`SELECT t.id FROM image_tags it
				 JOIN tags t             ON t.id  = it.tag_id
				 JOIN tag_categories tc  ON tc.id = t.category_id
				 WHERE it.image_id = ? AND t.name = ? AND tc.name = ?`,
				imageID, bareName, catName,
			).Scan(&tagID); err == nil {
				return tagID, nil
			}
		}
	}

	ids, err := db.QueryIDs(g.DB.Read,
		`SELECT t.id FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = ?`,
		imageID, tagName,
	)
	if err != nil {
		return 0, fmt.Errorf("tag lookup failed: %w", err)
	}
	switch len(ids) {
	case 0:
		return 0, nil
	case 1:
		return ids[0], nil
	default:
		return 0, fmt.Errorf("tag %q exists on this image in multiple categories; use category:name", tagName)
	}
}

func isMultipart(ct string) bool { return strings.HasPrefix(ct, "multipart/form-data") }
