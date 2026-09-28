package galleryio

import (
	"archive/zip"
	"context"
	"io"
	"os"

	"github.com/monbooru/monbooru/internal/logx"
)

type DownloadEntry struct {
	Path string
	Name string
}

// WriteDownload skips and counts a file a delete or move got to first;
// any other failure, or ctx, ends the zip.
func WriteDownload(ctx context.Context, w io.Writer, manifest []byte, entries []DownloadEntry, progress func(written int)) (written, skipped int, err error) {
	zw := zip.NewWriter(w)
	if manifest != nil {
		inner, err := zw.CreateHeader(&zip.FileHeader{Name: "tags.json", Method: zip.Deflate})
		if err != nil {
			return 0, 0, err
		}
		if _, err := inner.Write(manifest); err != nil {
			return 0, 0, err
		}
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return written, skipped, err
		}
		added, err := addDownloadFile(zw, e)
		if err != nil {
			return written, skipped, err
		}
		if !added {
			skipped++
			continue
		}
		written++
		if progress != nil {
			progress(written)
		}
	}
	return written, skipped, zw.Close()
}

func addDownloadFile(zw *zip.Writer, e DownloadEntry) (bool, error) {
	f, err := os.Open(e.Path)
	if err != nil {
		logx.Warnf("download: skip %q: %v", e.Path, err)
		return false, nil
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	// Media is already compressed; a deflate pass would spend CPU for nothing.
	entry, err := zw.CreateHeader(&zip.FileHeader{Name: e.Name, Method: zip.Store, Modified: info.ModTime()})
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(entry, f); err != nil {
		return false, err
	}
	return true, nil
}
