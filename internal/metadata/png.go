package metadata

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/monbooru/monbooru/internal/models"
)

var errNotPNG = errors.New("not a PNG file")

// Chunk lengths are attacker-controlled, up to 4 GiB; real parameter and
// workflow chunks are kilobytes.
const maxChunkBytes = 16 * 1024 * 1024

func readPNGTextChunks(r io.Reader) (map[string]string, error) {
	sig := make([]byte, 8)
	if _, err := io.ReadFull(r, sig); err != nil {
		return nil, errNotPNG
	}
	if sig[0] != 0x89 || sig[1] != 0x50 || sig[2] != 0x4E || sig[3] != 0x47 {
		return nil, errNotPNG
	}

	result := map[string]string{}

	for {
		header := make([]byte, 8)
		if _, err := io.ReadFull(r, header); err != nil {
			break
		}
		length := binary.BigEndian.Uint32(header[:4])
		chunkType := string(header[4:8])

		// Skipped unbuffered: the body plus its 4-byte CRC.
		if length > maxChunkBytes {
			if _, err := io.CopyN(io.Discard, r, int64(length)+4); err != nil {
				break
			}
			if chunkType == "IEND" {
				break
			}
			continue
		}

		data := make([]byte, length)
		if _, err := io.ReadFull(r, data); err != nil {
			break
		}

		crc := make([]byte, 4)
		if _, err := io.ReadFull(r, crc); err != nil {
			break
		}

		switch chunkType {
		case "tEXt":
			null := strings.IndexByte(string(data), 0)
			if null < 0 {
				continue
			}
			result[string(data[:null])] = string(data[null+1:])

		case "zTXt":
			// keyword \x00 cmethod text, the text always compressed
			null := strings.IndexByte(string(data), 0)
			if null < 0 || len(data) < null+2 {
				continue
			}
			text, err := inflateText(data[null+2:], data[null+1])
			if err != nil {
				continue
			}
			result[string(data[:null])] = string(text)

		case "iTXt":
			// keyword \x00 cflag cmethod lang \x00 tkeyword \x00 text
			null1 := strings.IndexByte(string(data), 0)
			if null1 < 0 {
				continue
			}
			key := string(data[:null1])
			rest := data[null1+1:]
			if len(rest) < 2 {
				continue
			}
			compressed, method := rest[0] == 1, rest[1]
			rest = rest[2:]
			null2 := strings.IndexByte(string(rest), 0)
			if null2 < 0 {
				continue
			}
			rest = rest[null2+1:]
			null3 := strings.IndexByte(string(rest), 0)
			if null3 < 0 {
				continue
			}
			text := rest[null3+1:]
			if compressed {
				inflated, err := inflateText(text, method)
				if err != nil {
					continue
				}
				text = inflated
			}
			result[key] = string(text)
		}

		if chunkType == "IEND" {
			break
		}
	}

	return result, nil
}

// Method 0, zlib, is the only one the PNG spec defines.
func inflateText(b []byte, method byte) ([]byte, error) {
	if method != 0 {
		return nil, errors.New("unknown iTXt compression method")
	}
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(io.LimitReader(zr, maxChunkBytes))
}

func extractFromPNG(path string) (*models.SDMetadata, *models.ComfyUIMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil
	}
	defer func() { _ = f.Close() }()

	chunks, err := readPNGTextChunks(f)
	if err != nil {
		return nil, nil, nil
	}

	var sd *models.SDMetadata
	var comfy *models.ComfyUIMetadata

	if text, ok := chunks["parameters"]; ok {
		sd = parseA1111Parameters(text)
	}

	// ComfyUI's API-format "prompt" wins; the node-graph "workflow" is
	// the fallback.
	if raw, ok := chunks["prompt"]; ok {
		comfy = parseComfyPromptChunk(raw)
	}
	if raw, ok := chunks["workflow"]; ok && comfy == nil {
		comfy = parseComfyWorkflow(raw)
	}

	return sd, comfy, nil
}
