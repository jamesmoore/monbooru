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
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
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

// SupportedMIMETypes lists bare .avif and .jxl so a picker on an OS that
// maps no type to them does not hide those files.
const SupportedMIMETypes = "image/jpeg,image/png,image/webp,image/avif,image/jxl,.avif,.jxl,image/gif,video/mp4,video/webm,application/vnd.comicbook+zip,application/zip,application/x-cbz"

// UniqueDestPath's stat check is racy: a caller that must not clobber a
// file opens with O_CREATE|O_EXCL.
func UniqueDestPath(destDir, filename string) string {
	return uniquePathBy(destDir, filename, uploadSuffix)
}

func uploadSuffix(stem, ext string, i int) string { return fmt.Sprintf("%s_%d%s", stem, i, ext) }

func uniquePathBy(dir, filename string, nameNth func(stem, ext string, i int) string) string {
	return uniquePathIn(dir, filename, nil, nameNth)
}

func uniquePathIn(dir, filename string, claimed map[string]struct{}, nameNth func(stem, ext string, i int) string) string {
	// Any stat error counts as free: a name the filesystem refuses would
	// be refused at every suffix, and the loop would never end.
	free := func(p string) bool {
		if _, taken := claimed[p]; taken {
			return false
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
	if t := ExtFileType(path); t != "" {
		return t, nil
	}
	return detectMagicType(path)
}

func ExtFileType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return models.FileTypeJPEG
	case ".png":
		return models.FileTypePNG
	case ".webp":
		return models.FileTypeWEBP
	case ".avif":
		return models.FileTypeAVIF
	case ".jxl":
		return models.FileTypeJXL
	case ".gif":
		return models.FileTypeGIF
	case ".mp4":
		return models.FileTypeMP4
	case ".webm":
		return models.FileTypeWEBM
	case ".cbz", ".zip":
		return models.FileTypeCBZ
	}
	return ""
}

func MagicFileType(path string) (string, error) { return detectMagicType(path) }

func detectMagicType(path string) (string, error) {
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

	if buf[0] == 0xFF && buf[1] == 0xD8 && buf[2] == 0xFF {
		return models.FileTypeJPEG, nil
	}
	if len(buf) >= 8 &&
		buf[0] == 0x89 && buf[1] == 0x50 && buf[2] == 0x4E && buf[3] == 0x47 &&
		buf[4] == 0x0D && buf[5] == 0x0A && buf[6] == 0x1A && buf[7] == 0x0A {
		return models.FileTypePNG, nil
	}
	if buf[0] == 0x47 && buf[1] == 0x49 && buf[2] == 0x46 && buf[3] == 0x38 {
		return models.FileTypeGIF, nil
	}
	if len(buf) >= 12 &&
		buf[0] == 0x52 && buf[1] == 0x49 && buf[2] == 0x46 && buf[3] == 0x46 &&
		buf[8] == 0x57 && buf[9] == 0x45 && buf[10] == 0x42 && buf[11] == 0x50 {
		return models.FileTypeWEBP, nil
	}
	if (buf[0] == 0xFF && buf[1] == 0x0A) || bytes.HasPrefix(buf, jxlContainer) {
		return models.FileTypeJXL, nil
	}
	// Brands, not the bare ftyp box: .mov, .heic and .3gp would pass as
	// MP4 and fail in the browser. AVIF is asked first, by its own
	// brands: the mif1 it also lists is HEIC's too.
	if len(buf) >= 12 && buf[4] == 0x66 && buf[5] == 0x74 && buf[6] == 0x79 && buf[7] == 0x70 {
		if hasFtypBrand(buf, "avif", "avis") {
			return models.FileTypeAVIF, nil
		}
		if hasFtypBrand(buf, "mp42", "mp41", "isom", "iso2", "avc1") {
			return models.FileTypeMP4, nil
		}
	}
	if buf[0] == 0x1A && buf[1] == 0x45 && buf[2] == 0xDF && buf[3] == 0xA3 {
		return models.FileTypeWEBM, nil
	}
	if buf[0] == 0x50 && buf[1] == 0x4B &&
		(buf[2] == 0x03 && buf[3] == 0x04 || buf[2] == 0x05 && buf[3] == 0x06) {
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
	return fileType == models.FileTypeMP4 || fileType == models.FileTypeWEBM
}

// IsFFmpegStill reports the still types Go's image packages cannot decode.
func IsFFmpegStill(fileType string) bool {
	return fileType == models.FileTypeAVIF || fileType == models.FileTypeJXL
}

// Only for files monbooru names itself; an operator's file keeps its name.
func extForFileType(fileType string) string { return fileTypeMeta[fileType].ext }

// MIMEForFileType exists because http.ServeFile goes by the extension,
// which the bytes can contradict.
func MIMEForFileType(fileType string) string { return fileTypeMeta[fileType].mime }

var fileTypeMeta = map[string]struct{ ext, mime string }{
	models.FileTypeJPEG: {".jpg", "image/jpeg"},
	models.FileTypePNG:  {".png", "image/png"},
	models.FileTypeWEBP: {".webp", "image/webp"},
	models.FileTypeAVIF: {".avif", "image/avif"},
	models.FileTypeJXL:  {".jxl", "image/jxl"},
	models.FileTypeGIF:  {".gif", "image/gif"},
	models.FileTypeMP4:  {".mp4", "video/mp4"},
	models.FileTypeWEBM: {".webm", "video/webm"},
	// CBZ serves as what the stdlib sniffer already answers for a PK archive.
	models.FileTypeCBZ: {".cbz", "application/zip"},
}

// ContentDispositionFor names the download: the byte routes have no
// extension, and the Content-Type cannot tell .cbz from .zip. Inline keeps
// the URL usable as an <img> or <video> src.
func ContentDispositionFor(canonPath string) string {
	return mime.FormatMediaType("inline", map[string]string{"filename": filepath.Base(canonPath)})
}
