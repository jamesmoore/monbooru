package gallery

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/models"
)

func resolveSubdir(galleryPath, folder string) (string, error) {
	folder = strings.TrimSpace(folder)
	if folder == "" {
		return galleryPath, nil
	}
	// Before the trim, which would turn "/tmp/x" into a relative "tmp/x".
	if filepath.IsAbs(folder) {
		return "", fmt.Errorf("folder must be relative to the gallery root")
	}
	folder = strings.Trim(folder, "/\\")
	cleaned := filepath.Clean(filepath.ToSlash(folder))
	if !filepath.IsLocal(cleaned) {
		return "", fmt.Errorf("folder path escapes the gallery root")
	}
	abs, err := filepath.Abs(filepath.Join(galleryPath, cleaned))
	if err != nil {
		return "", err
	}
	galleryAbs, err := filepath.Abs(galleryPath)
	if err != nil {
		return "", err
	}
	if !PathInside(galleryAbs, abs) {
		return "", fmt.Errorf("folder path escapes the gallery root")
	}
	return abs, nil
}

// PathInside wants both paths clean and absolute, and counts root itself
// as inside. Rel, not a prefix test, which /data/gallery_backup would pass
// for /data/gallery.
func PathInside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && !climbsOut(rel)
}

// A name that only starts with two dots, like "..drafts", is inside.
func climbsOut(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// NamedInside checks the name, not the resolved path: bytes behind a
// symlinked folder belong to the gallery. It trusts that only monbooru
// writes the stored paths it gates.
func NamedInside(root, target string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	return PathInside(rootAbs, targetAbs)
}

var ErrUnsupportedType = errors.New("unsupported file type")

var SupportedMIMETypes = func() string {
	var tokens []string
	for _, ft := range models.FileTypes {
		tokens = append(append(tokens, ft.MIME), ft.Accept...)
	}
	return strings.Join(tokens, ",")
}()

// CreateUnique creates an empty file under filename, suffixed while the
// name is taken, and returns its path: the caller writes over its own
// reservation, so two writers never share one name.
func CreateUnique(dir, filename string) (string, error) {
	return createUniqueBy(dir, filename, uploadSuffix)
}

func createUniqueBy(dir, filename string, nameNth func(stem, ext string, i int) string) (string, error) {
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	p := filepath.Join(dir, filename)
	for i := 1; ; i++ {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err == nil {
			if err := f.Close(); err != nil {
				_ = os.Remove(p)
				return "", err
			}
			return p, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		p = filepath.Join(dir, nameNth(stem, ext, i))
	}
}

func uploadSuffix(stem, ext string, i int) string { return fmt.Sprintf("%s_%d%s", stem, i, ext) }

func uniquePathIn(dir, filename string, planned map[string]bool, nameNth func(stem, ext string, i int) string) string {
	// Any stat error counts as free: a name the filesystem refuses would
	// be refused at every suffix, and the loop would never end.
	free := func(p string) bool {
		if taken, ok := planned[p]; ok {
			return !taken
		}
		_, err := os.Stat(p)
		return err != nil
	}
	dst := filepath.Join(dir, filename)
	if free(dst) {
		return dst
	}
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	for i := 1; ; i++ {
		if candidate := filepath.Join(dir, nameNth(stem, ext, i)); free(candidate) {
			return candidate
		}
	}
}

type cancellableReader struct {
	ctx context.Context
	r   io.Reader
}

func (c cancellableReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func streamFile(ctx context.Context, path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening file for hashing: %w", err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 32*1024)
	if _, err := io.CopyBuffer(w, cancellableReader{ctx, f}, buf); err != nil {
		return fmt.Errorf("hashing file: %w", err)
	}
	return nil
}

func hashFileWith(ctx context.Context, path string, h hash.Hash) (string, error) {
	if err := streamFile(ctx, path, h); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func HashFile(path string) (string, error) {
	return hashFileWith(context.Background(), path, sha256.New())
}

// Md5File is for booru lookups only; sha256 stays the content address and
// the dedup key.
func Md5File(path string) (string, error) { return hashFileWith(context.Background(), path, md5.New()) }

// One read for both digests, so the md5 a booru lookup searches by always
// describes the sha256's bytes.
func hashFileDigests(path string) (sha, sum string, err error) {
	shaH, md5H := sha256.New(), md5.New()
	if err := streamFile(context.Background(), path, io.MultiWriter(shaH, md5H)); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(shaH.Sum(nil)), hex.EncodeToString(md5H.Sum(nil)), nil
}

// DetectFileType trusts the extension so the sync walk opens no file it
// can name.
func DetectFileType(path string) (string, error) {
	if t := models.ExtFileType(path); t != "" {
		return t, nil
	}
	return MagicFileType(path)
}

func MagicFileType(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", ErrUnsupportedType
	}
	defer func() { _ = f.Close() }()

	// Enough for the longest ftyp brand list in practice; every other
	// signature fits in 12.
	buf := make([]byte, 64)
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]

	return detectMagic(buf)
}

func detectMagic(buf []byte) (string, error) {
	if len(buf) < 4 {
		return "", ErrUnsupportedType
	}

	if bytes.HasPrefix(buf, []byte{0xFF, 0xD8, 0xFF}) {
		return models.FileTypeJPEG, nil
	}
	if bytes.HasPrefix(buf, []byte("\x89PNG\r\n\x1a\n")) {
		return models.FileTypePNG, nil
	}
	if bytes.HasPrefix(buf, []byte("GIF8")) {
		return models.FileTypeGIF, nil
	}
	if len(buf) >= 12 && string(buf[:4]) == "RIFF" && string(buf[8:12]) == "WEBP" {
		return models.FileTypeWEBP, nil
	}
	if bytes.HasPrefix(buf, []byte{0xFF, 0x0A}) || bytes.HasPrefix(buf, jxlContainer) {
		return models.FileTypeJXL, nil
	}
	// Brands, not the bare ftyp box: .mov, .heic and .3gp would pass as
	// MP4 and fail in the browser. AVIF is asked first, by its own
	// brands: the mif1 it also lists is HEIC's too.
	if len(buf) >= 12 && string(buf[4:8]) == "ftyp" {
		if hasFtypBrand(buf, "avif", "avis") {
			return models.FileTypeAVIF, nil
		}
		if hasFtypBrand(buf, "mp42", "mp41", "isom", "iso2", "avc1") {
			return models.FileTypeMP4, nil
		}
	}
	if bytes.HasPrefix(buf, []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		return models.FileTypeWEBM, nil
	}
	if bytes.HasPrefix(buf, []byte("PK\x03\x04")) || bytes.HasPrefix(buf, []byte("PK\x05\x06")) {
		return models.FileTypeCBZ, nil
	}

	return "", ErrUnsupportedType
}

var jxlContainer = []byte{0x00, 0x00, 0x00, 0x0C, 'J', 'X', 'L', ' ', 0x0D, 0x0A, 0x87, 0x0A}

// Compatible brands count as much as the major one: danbooru's mp4s are
// major iso5 and name mp41 only there.
func hasFtypBrand(buf []byte, brands ...string) bool {
	end := min(int(binary.BigEndian.Uint32(buf[:4])), len(buf))
	for off := 8; off+4 <= end; off += 4 {
		if off == 12 {
			continue // minor_version
		}
		if slices.Contains(brands, string(buf[off:off+4])) {
			return true
		}
	}
	return false
}

func IsVideoType(fileType string) bool {
	ft, _ := models.LookupFileType(fileType)
	return ft.Video
}

// IsFFmpegStill reports the still types Go's image packages cannot decode.
func IsFFmpegStill(fileType string) bool {
	ft, _ := models.LookupFileType(fileType)
	return ft.FFmpegStill
}

// Only for files monbooru names itself; an operator's file keeps its name.
func extForFileType(fileType string) string {
	ft, ok := models.LookupFileType(fileType)
	if !ok {
		return ""
	}
	return ft.Exts[0]
}

// MIMEForFileType exists because http.ServeFile goes by the extension,
// which the bytes can contradict.
func MIMEForFileType(fileType string) string {
	ft, _ := models.LookupFileType(fileType)
	return ft.MIME
}

// ContentDispositionFor names the download: the byte routes have no
// extension, and the Content-Type cannot tell .cbz from .zip. Inline keeps
// the URL usable as an <img> or <video> src.
func ContentDispositionFor(canonPath string) string {
	return mime.FormatMediaType("inline", map[string]string{"filename": filepath.Base(canonPath)})
}
