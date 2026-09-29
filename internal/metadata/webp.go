package metadata

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"

	"github.com/monbooru/monbooru/internal/models"
)

var exifMagic = []byte("Exif\x00\x00")

func extractSDFromWebP(path string) *models.SDMetadata {
	x, err := decodeWebPEXIF(path)
	if err != nil || x == nil {
		return nil
	}
	return sdFromEXIF(x)
}

func decodeWebPEXIF(path string) (*exifData, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	exifData, err := readWebPEXIF(f)
	if err != nil || exifData == nil {
		return nil, err
	}
	return decodeEXIF(exifData)
}

func readWebPEXIF(r io.Reader) ([]byte, error) {
	header := make([]byte, 12)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WEBP" {
		return nil, nil
	}
	for {
		chunk := make([]byte, 8)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, nil
		}
		chunkType := string(chunk[0:4])
		size := binary.LittleEndian.Uint32(chunk[4:8])
		if size > maxChunkBytes {
			toSkip := int64(size)
			if size%2 == 1 {
				toSkip++
			}
			if _, err := io.CopyN(io.Discard, r, toSkip); err != nil {
				return nil, nil
			}
			continue
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, nil
		}
		if size%2 == 1 {
			// RIFF chunks are word-aligned; skip the pad byte.
			pad := make([]byte, 1)
			_, _ = io.ReadFull(r, pad)
		}
		if chunkType == "EXIF" {
			// Some encoders prepend the JPEG APP1 header; decodeEXIF
			// wants the bare TIFF payload.
			data = bytes.TrimPrefix(data, exifMagic)
			return data, nil
		}
	}
}

func genericFromWebP(path string) []models.SDParam {
	x, err := decodeWebPEXIF(path)
	if err != nil || x == nil {
		return nil
	}
	return collectEXIFTags(x)
}
