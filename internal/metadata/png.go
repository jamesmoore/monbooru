package metadata

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"os"

	"github.com/monbooru/monbooru/internal/models"
)

var errNotPNG = errors.New("not a PNG file")

// Chunk lengths are attacker-controlled, up to 4 GiB; real parameter and
// workflow chunks are kilobytes.
const maxChunkBytes = 16 * 1024 * 1024

// The text one PNG keeps, every chunk together: a file can hold any number
// of chunks under the per-chunk cap.
const maxPNGTextBytes = 16 * 1024 * 1024

func readPNGTextChunks(r io.Reader) (map[string]string, error) {
	sig := make([]byte, 8)
	if _, err := io.ReadFull(r, sig); err != nil {
		return nil, errNotPNG
	}
	if sig[0] != 0x89 || sig[1] != 0x50 || sig[2] != 0x4E || sig[3] != 0x47 {
		return nil, errNotPNG
	}

	result := map[string]string{}
	budget := int64(maxPNGTextBytes)

	for {
		header := make([]byte, 8)
		if _, err := io.ReadFull(r, header); err != nil {
			break
		}
		length := binary.BigEndian.Uint32(header[:4])
		chunkType := string(header[4:8])

		// Skipped unbuffered: the body plus its 4-byte CRC.
		textChunk := chunkType == "tEXt" || chunkType == "zTXt" || chunkType == "iTXt"
		if !textChunk || int64(length) > budget {
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
			key, text, ok := bytes.Cut(data, []byte{0})
			if !ok {
				continue
			}
			result[string(key)] = string(text)
			budget -= int64(len(data))

		case "zTXt":
			// keyword \x00 cmethod text, the text always compressed
			key, rest, ok := bytes.Cut(data, []byte{0})
			if !ok || len(rest) < 1 {
				continue
			}
			text, err := inflateText(rest[1:], rest[0], budget-int64(len(key)))
			if err != nil {
				continue
			}
			result[string(key)] = string(text)
			budget -= int64(len(key) + len(text))

		case "iTXt":
			// keyword \x00 cflag cmethod lang \x00 tkeyword \x00 text
			key, rest, ok := bytes.Cut(data, []byte{0})
			if !ok || len(rest) < 2 {
				continue
			}
			compressed, method := rest[0] == 1, rest[1]
			if _, rest, ok = bytes.Cut(rest[2:], []byte{0}); !ok {
				continue
			}
			_, text, ok := bytes.Cut(rest, []byte{0})
			if !ok {
				continue
			}
			if compressed {
				inflated, err := inflateText(text, method, budget-int64(len(key)))
				if err != nil {
					continue
				}
				text = inflated
			}
			result[string(key)] = string(text)
			budget -= int64(len(key) + len(text))
		}

		if chunkType == "IEND" {
			break
		}
	}

	return result, nil
}

// Method 0, zlib, is the only one the PNG spec defines.
func inflateText(b []byte, method byte, limit int64) ([]byte, error) {
	if method != 0 {
		return nil, errors.New("unknown iTXt compression method")
	}
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(io.LimitReader(zr, limit))
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
