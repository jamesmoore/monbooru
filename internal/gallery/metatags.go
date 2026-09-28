package gallery

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"image/jpeg"
	"io"
	"math"
	"os"
	"slices"
	"sync/atomic"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// MetaTagsEnabled gates only the automatic derivation; the Maintenance
// actions run whatever it holds.
var MetaTagsEnabled atomic.Bool

func init() { MetaTagsEnabled.Store(true) }

type MetaFacts struct {
	Width, Height int
	FileType      string
	AnimatedGIF   bool
	AnimatedPNG   bool
	AnimatedWebP  bool
	AnimatedAVIF  bool
	AnimatedJXL   bool
	HasSound      bool
	Greyscale     bool
	EXIFRotated   bool
	AIGenerated   bool
}

// MetaTagsFor follows Danbooru's names, thresholds and cumulative
// resolution bands, so a pulled post and a derived row never disagree.
func MetaTagsFor(f MetaFacts) []string {
	var out []string
	if w, h := f.Width, f.Height; w > 0 && h > 0 {
		if w >= 1600 || h >= 1200 {
			out = append(out, "highres")
		}
		if w >= 3200 || h >= 2400 {
			out = append(out, "absurdres")
		}
		if w >= 10000 || h >= 10000 {
			out = append(out, "incredibly_absurdres")
		}
		if w <= 500 && h <= 500 {
			out = append(out, "lowres")
		}
		if w >= 1024 && w >= h*4 {
			out = append(out, "wide_image")
		} else if h >= 1024 && h >= w*4 {
			out = append(out, "tall_image")
		}
	}
	video := IsVideoType(f.FileType)
	if video {
		out = append(out, "video")
	}
	if f.AnimatedGIF {
		out = append(out, "animated_gif")
	}
	if f.AnimatedPNG {
		out = append(out, "animated_png")
	}
	if video || f.AnimatedGIF || f.AnimatedPNG || f.AnimatedWebP || f.AnimatedAVIF || f.AnimatedJXL {
		out = append(out, "animated")
	}
	if f.HasSound {
		out = append(out, "sound")
	}
	if f.Greyscale {
		out = append(out, "greyscale")
	}
	if f.EXIFRotated {
		out = append(out, "exif_rotation")
	}
	if f.AIGenerated {
		out = append(out, "ai-generated")
	}
	slices.Sort(out)
	return out
}

func ApplyMetaTags(database *db.DB, thumbnailsPath string, imageID int64) error {
	if !MetaTagsEnabled.Load() {
		return nil
	}
	metaCat, err := metaCategoryID(database)
	if err != nil {
		return err
	}
	_, err = applyMetaTags(database, tags.New(database), metaCat, thumbnailsPath, imageID)
	return err
}

func BackfillMetaTags(ctx context.Context, database *db.DB, thumbnailsPath string, progress func(processed, total int, message string)) (processed, updated int, err error) {
	metaCat, err := metaCategoryID(database)
	if err != nil {
		return 0, 0, err
	}
	ids, err := db.QueryIDs(database.Read, `SELECT id FROM images WHERE is_missing = 0 ORDER BY id`)
	if err != nil {
		return 0, 0, err
	}
	svc := tags.New(database)
	changed := 0
	processed, _, err = BackfillWalk(ctx, ids, progress, "meta tags", "Meta tags…", func(id int64) error {
		moved, err := applyMetaTags(database, svc, metaCat, thumbnailsPath, id)
		if moved {
			changed++
		}
		return err
	})
	return processed, changed, err
}

func RemoveMetaTags(ctx context.Context, database *db.DB, progress func(processed, total int, message string)) (processed, removed int, err error) {
	ids, err := db.QueryIDs(database.Read,
		`SELECT DISTINCT image_id FROM image_tag_sources WHERE source = ? ORDER BY image_id`, models.TagSourceMonbooru)
	if err != nil {
		return 0, 0, err
	}
	svc := tags.New(database)
	dropped := 0
	processed, _, err = BackfillWalk(ctx, ids, progress, "removing meta tags", "Meta tags…", func(id int64) error {
		_, n, err := svc.DropSourceFromImageTags(id, models.TagSourceMonbooru, nil)
		dropped += n
		return err
	})
	return processed, dropped, err
}

func metaCategoryID(database *db.DB) (int64, error) {
	id, ok, err := tags.CategoryIDByName(database, "meta")
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errors.New("meta category missing")
	}
	return id, nil
}

func applyMetaTags(database *db.DB, tagSvc *tags.Service, metaCatID int64, thumbnailsPath string, imageID int64) (bool, error) {
	facts, err := metaFactsFor(database, thumbnailsPath, imageID)
	if err != nil {
		return false, err
	}
	want := MetaTagsFor(facts)
	held, err := heldMetaTags(database, imageID)
	if err != nil {
		return false, err
	}
	// By id, not name: an aliased or merged name resolves to a row under
	// another name, which a by-name diff would drop and re-add every pass.
	resolved, err := resolveMetaNames(database, metaCatID, want)
	if err != nil {
		return false, err
	}
	wanted := make(map[int64]bool, len(want))
	var add []string
	for _, name := range want {
		id, ok := resolved[name]
		if ok {
			wanted[id] = true
		}
		if !ok || !held[id] {
			add = append(add, name)
		}
	}
	var drop []int64
	for id := range held {
		if !wanted[id] {
			drop = append(drop, id)
		}
	}
	var added []int64
	if len(add) > 0 {
		ids, err := tagSvc.GetOrCreateTagsFrom(add, metaCatID, models.TagSourceMonbooru)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			if !held[id] {
				added = append(added, id)
			}
		}
	}
	if len(added) > 0 {
		// Reconcile off: a derived tag is re-asserted or withdrawn, never
		// left stale like a source's dropped tag.
		if _, err := tagSvc.SyncSourceTags(imageID, added, models.TagSourceMonbooru, false); err != nil {
			return false, err
		}
	}
	if len(drop) > 0 {
		if _, _, err := tagSvc.DropSourceFromImageTags(imageID, models.TagSourceMonbooru, drop); err != nil {
			return false, err
		}
	}
	return len(added) > 0 || len(drop) > 0, nil
}

// Every claim this package writes has a ledger row, so the ledger alone
// is the whole claim.
func heldMetaTags(database *db.DB, imageID int64) (map[int64]bool, error) {
	ids, err := db.QueryIDs(database.Read,
		`SELECT tag_id FROM image_tag_sources WHERE image_id = ? AND source = ?`, imageID, models.TagSourceMonbooru)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// Names with no row yet are left out; creating them is the caller's write.
func resolveMetaNames(database *db.DB, metaCatID int64, names []string) (map[string]int64, error) {
	out := make(map[string]int64, len(names))
	if len(names) == 0 {
		return out, nil
	}
	placeholders, args := db.InPlaceholders(names)
	rows, err := database.Read.Query(
		`SELECT name, COALESCE(canonical_tag_id, id) FROM tags WHERE category_id = ? AND name IN (`+placeholders+`)`,
		append([]any{metaCatID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var id int64
		if err := rows.Scan(&name, &id); err != nil {
			return nil, err
		}
		out[name] = id
	}
	return out, rows.Err()
}

func metaFactsFor(database *db.DB, thumbnailsPath string, imageID int64) (MetaFacts, error) {
	var f MetaFacts
	var w, h sql.NullInt64
	var path, sourceType string
	if err := database.Read.QueryRow(
		`SELECT canonical_path, file_type, source_type, width, height FROM images WHERE id = ?`, imageID,
	).Scan(&path, &f.FileType, &sourceType, &w, &h); err != nil {
		return f, err
	}
	f.Width, f.Height = int(w.Int64), int(h.Int64)
	f.AIGenerated = sourceType != models.SourceTypeNone
	f.Greyscale = greyscaleThumb(ThumbnailPath(thumbnailsPath, imageID))
	switch f.FileType {
	case models.FileTypeGIF:
		f.AnimatedGIF = animatedGIF(path)
	case models.FileTypePNG:
		f.AnimatedPNG = animatedPNG(path)
	case models.FileTypeAVIF:
		f.AnimatedAVIF = animatedAVIF(path)
	case models.FileTypeJXL:
		f.AnimatedJXL = animatedJXL(path)
	case models.FileTypeJPEG, models.FileTypeWEBP:
		if o, ok := metadata.EXIFOrientation(path, f.FileType); ok {
			f.EXIFRotated = o != 1
		}
		f.AnimatedWebP = f.FileType == models.FileTypeWEBP && animatedWebP(path)
	case models.FileTypeMP4, models.FileTypeWEBM:
		f.HasSound = ProbeVideoHasAudio(path)
	}
	return f, nil
}

// The spread absorbs the chroma ringing a Q85 thumbnail shows at edges; a
// tinted image (sepia) still fails.
const (
	greyChannelSpread = 8
	greyPixelFraction = 0.99
)

// The thumbnail, not the original: one small decode that works for every
// file type, videos and archives included.
func greyscaleThumb(thumbPath string) bool {
	f, err := os.Open(thumbPath)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	img, err := jpeg.Decode(f)
	if err != nil {
		return false
	}
	b := img.Bounds()
	total, grey := 0, 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			r8, g8, b8 := int(r>>8), int(g>>8), int(bl>>8)
			total++
			if max(r8, g8, b8)-min(r8, g8, b8) <= greyChannelSpread {
				grey++
			}
		}
	}
	return total > 0 && float64(grey) >= float64(total)*greyPixelFraction
}

// Seeks past the payloads rather than decoding them: one frame's LZW data
// can run to megabytes.
func animatedGIF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 13)
	if _, err := io.ReadFull(f, head); err != nil || string(head[:3]) != "GIF" {
		return false
	}
	if head[10]&0x80 != 0 && !skip(f, colorTableBytes(head[10])) {
		return false
	}
	frames := 0
	block := make([]byte, 1)
	for {
		if _, err := io.ReadFull(f, block); err != nil {
			return false
		}
		switch block[0] {
		case 0x2C:
			desc := make([]byte, 9)
			if _, err := io.ReadFull(f, desc); err != nil {
				return false
			}
			if desc[8]&0x80 != 0 && !skip(f, colorTableBytes(desc[8])) {
				return false
			}
			// The LZW minimum code size byte precedes the sub-blocks.
			if !skip(f, 1) {
				return false
			}
			frames++
			if frames > 1 {
				return true
			}
			if !skipSubBlocks(f) {
				return false
			}
		case 0x21:
			if !skip(f, 1) || !skipSubBlocks(f) {
				return false
			}
		default:
			// The trailer, or bytes that are no longer a block header.
			return false
		}
	}
}

func colorTableBytes(packed byte) int64 { return 3 << ((packed & 0x07) + 1) }

func skip(f *os.File, n int64) bool {
	_, err := f.Seek(n, io.SeekCurrent)
	return err == nil
}

func skipSubBlocks(f *os.File) bool {
	size := make([]byte, 1)
	for {
		if _, err := io.ReadFull(f, size); err != nil {
			return false
		}
		if size[0] == 0 {
			return true
		}
		if !skip(f, int64(size[0])) {
			return false
		}
	}
}

// The APNG spec puts acTL ahead of the first IDAT.
func animatedPNG(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sig := make([]byte, 8)
	if _, err := io.ReadFull(f, sig); err != nil || string(sig[1:4]) != "PNG" {
		return false
	}
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(f, hdr); err != nil {
			return false
		}
		switch string(hdr[4:8]) {
		case "acTL":
			return true
		case "IDAT", "IEND":
			return false
		}
		// The chunk's declared length, plus its trailing CRC.
		if !skip(f, int64(binary.BigEndian.Uint32(hdr[:4]))+4) {
			return false
		}
	}
}

// Danbooru counts the frames of a WebP too, each an ANMF chunk.
func animatedWebP(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 12)
	if _, err := io.ReadFull(f, head); err != nil || string(head[8:]) != "WEBP" {
		return false
	}
	frames := 0
	chunk := make([]byte, 8)
	for {
		if _, err := io.ReadFull(f, chunk); err != nil {
			return false
		}
		if string(chunk[:4]) == "ANMF" {
			frames++
			if frames > 1 {
				return true
			}
		}
		// A payload of odd length is padded to an even one.
		size := int64(binary.LittleEndian.Uint32(chunk[4:]))
		if !skip(f, size+size%2) {
			return false
		}
	}
}

// The image sequence brand is also what Danbooru's is_animated_avif? asks.
func animatedAVIF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 64)
	n, _ := io.ReadFull(f, buf)
	return n >= 12 && string(buf[4:8]) == "ftyp" && hasFtypBrand(buf[:n], "avis")
}

// A container carries the codestream in a jxlc box, or split over jxlp
// boxes that each open on a four-byte index.
func animatedJXL(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, len(jxlContainer))
	n, _ := io.ReadFull(f, head)
	if !bytes.Equal(head[:n], jxlContainer) {
		return jxlHasAnimation(io.MultiReader(bytes.NewReader(head[:n]), f))
	}
	box := make([]byte, 8)
	for {
		if _, err := io.ReadFull(f, box); err != nil {
			return false
		}
		size := int64(binary.BigEndian.Uint32(box))
		if size == 0 {
			size = math.MaxInt64 // the box runs to the end of the file
		}
		switch string(box[4:]) {
		case "jxlc":
			return jxlHasAnimation(io.LimitReader(f, size-8))
		case "jxlp":
			return skip(f, 4) && jxlHasAnimation(io.LimitReader(f, size-12))
		}
		if size < 8 || !skip(f, size-8) {
			return false
		}
	}
}

// jxlHasAnimation reads the codestream's SizeHeader and ImageMetadata
// (ISO/IEC 18181-1) as far as have_animation.
func jxlHasAnimation(r io.Reader) bool {
	// At most 192 bits precede the flag.
	buf := make([]byte, 24)
	n, _ := io.ReadFull(r, buf)
	b := jxlBits{buf: buf[:n]}
	if b.read(16) != 0x0AFF {
		return false
	}
	b.size()
	if b.read(1) == 1 || b.read(1) == 0 { // all_default, extra_fields
		return false
	}
	b.read(3) // orientation
	if b.read(1) == 1 {
		b.size() // intrinsic size
	}
	if b.read(1) == 1 {
		b.preview()
	}
	return b.read(1) == 1
}

// jxlBits reads least significant bit first. Past the end it reads zeros,
// so a header cut short can only answer no.
type jxlBits struct {
	buf []byte
	pos int
}

func (b *jxlBits) read(n int) uint32 {
	var v uint32
	for i := range n {
		if at := b.pos / 8; at < len(b.buf) {
			v |= uint32(b.buf[at]>>(b.pos%8)&1) << i
		}
		b.pos++
	}
	return v
}

// u32 skips a U32 field, whose two-bit selector picks one of four widths.
func (b *jxlBits) u32(widths [4]int) { b.read(widths[b.read(2)]) }

func (b *jxlBits) size() {
	small := b.read(1) == 1
	dim := func() {
		if small {
			b.read(5)
		} else {
			b.u32([4]int{9, 13, 18, 30})
		}
	}
	dim()
	if b.read(3) == 0 { // no ratio: the width is coded too
		dim()
	}
}

func (b *jxlBits) preview() {
	widths := [4]int{6, 8, 10, 12}
	if b.read(1) == 1 { // div8
		widths = [4]int{0, 0, 5, 9}
	}
	b.u32(widths)
	if b.read(3) == 0 {
		b.u32(widths)
	}
}
